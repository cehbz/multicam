package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeCamera simulates a legacy-API body: the rec-mode API list appears only
// after startRecMode and a NotReady interval, startMovieRec only in movie mode.
type fakeCamera struct {
	t          *testing.T
	srv        *httptest.Server
	mu         sync.Mutex
	recMode    bool
	readyPolls int
	shootMode  string
	recording  bool
	zoom       int
	calls      []string
	jpeg       []byte
}

func newFakeCamera(t *testing.T) *fakeCamera {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 64, 48)), nil); err != nil {
		t.Fatal(err)
	}
	c := &fakeCamera{t: t, shootMode: "still", jpeg: buf.Bytes()}
	mux := http.NewServeMux()
	mux.HandleFunc("/sony/camera", c.rpc)
	mux.HandleFunc("/liveview/liveviewstream", c.liveview)
	c.srv = httptest.NewServer(mux)
	t.Cleanup(c.srv.Close)
	return c
}

func (c *fakeCamera) apis() []string {
	l := []string{"getVersions", "getMethodTypes", "getApplicationInfo", "getAvailableApiList", "getEvent", "startRecMode", "stopRecMode"}
	if c.recMode && c.readyPolls <= 0 {
		l = append(l, "setShootMode", "getAvailableShootMode", "startLiveview", "stopLiveview",
			"startLiveviewWithSize", "getSupportedLiveviewSize", "getAvailableFNumber", "getSupportedFNumber", "setFNumber", "actZoom")
		if c.shootMode == "movie" {
			l = append(l, "startMovieRec", "stopMovieRec")
		}
	}
	return l
}

func (c *fakeCamera) rpc(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Method  string
		Params  []any
		ID      int
		Version string
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		c.t.Errorf("bad request body: %v", err)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, req.Method+"@"+req.Version)
	reply := func(key string, v any) {
		json.NewEncoder(w).Encode(map[string]any{key: v, "id": req.ID})
	}
	switch req.Method {
	case "getVersions":
		reply("result", []any{[]string{"1.0", "1.1"}})
	case "getMethodTypes":
		if req.Params[0] == "1.1" {
			reply("results", [][]any{{"getEvent", []string{"bool"}, []string{"json*"}, "1.1"}})
			return
		}
		reply("results", [][]any{
			{"getEvent", []string{"bool"}, []string{"json*"}, "1.0"},
			{"startMovieRec", []string{}, []string{"int"}, "1.0"},
			{"actZoom", []string{"string", "string"}, []string{"int"}, "1.0"},
		})
	case "getApplicationInfo":
		reply("result", []string{"Smart Remote Control", "2.1.4"})
	case "getAvailableApiList":
		if c.recMode && c.readyPolls > 0 {
			c.readyPolls--
		}
		reply("result", []any{c.apis()})
	case "getEvent":
		if len(req.Params) != 1 || req.Params[0] != false {
			c.t.Errorf("getEvent params %v, want [false]", req.Params)
		}
		status := "IDLE"
		switch {
		case c.recMode && c.readyPolls > 0:
			status = "NotReady"
		case c.recording:
			status = "MovieRecording"
		}
		reply("result", []any{
			map[string]any{"type": "availableApiList", "names": c.apis()},
			map[string]any{"type": "cameraStatus", "cameraStatus": status},
			map[string]any{"type": "zoomInformation", "zoomPosition": c.zoom},
			nil,
			map[string]any{"type": "shootMode", "currentShootMode": c.shootMode},
		})
	case "startRecMode":
		c.recMode, c.readyPolls = true, 2
		reply("result", []int{0})
	case "setShootMode":
		c.shootMode = req.Params[0].(string)
		reply("result", []int{0})
	case "getAvailableFNumber":
		reply("result", []any{"2.8", []string{"2.8", "4.0"}})
	case "getSupportedLiveviewSize":
		reply("result", []any{[]string{"M"}})
	case "startMovieRec":
		c.recording = true
		reply("result", []int{0})
	case "stopMovieRec":
		c.recording = false
		reply("result", []string{""})
	case "actZoom":
		if req.Params[0] == "in" && req.Params[1] == "start" {
			c.zoom = 50
		}
		if req.Params[0] == "out" && req.Params[1] == "start" {
			c.zoom = 10
		}
		reply("result", []int{0})
	case "startLiveview", "startLiveviewWithSize":
		reply("result", []string{c.srv.URL + "/liveview/liveviewstream"})
	case "stopLiveview":
		reply("result", []int{0})
	default:
		reply("error", []any{12, "No Such Method"})
	}
}

func (c *fakeCamera) liveview(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/octet-stream")
	for seq := uint16(1); ; seq++ {
		var p bytes.Buffer
		p.Write([]byte{0xFF, 0x01})
		binary.Write(&p, binary.BigEndian, seq)
		binary.Write(&p, binary.BigEndian, uint32(seq)*40)
		hdr := make([]byte, 128)
		copy(hdr, []byte{0x24, 0x35, 0x68, 0x79})
		n := len(c.jpeg)
		hdr[4], hdr[5], hdr[6], hdr[7] = byte(n>>16), byte(n>>8), byte(n), 3
		p.Write(hdr)
		p.Write(c.jpeg)
		p.Write([]byte{0, 0, 0})
		if _, err := w.Write(p.Bytes()); err != nil {
			return
		}
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func testOptions(t *testing.T) options {
	o := defaultOptions()
	o.body = "fake"
	o.out = t.TempDir()
	o.ssdp = false
	o.liveview = 300 * time.Millisecond
	o.poll = 10 * time.Millisecond
	o.hold = 50 * time.Millisecond
	o.readyTimeout = 2 * time.Second
	return o
}

func runDir(t *testing.T, o options) string {
	t.Helper()
	dirs, _ := filepath.Glob(filepath.Join(o.out, o.body, "*"))
	if len(dirs) != 1 {
		t.Fatalf("capture dirs %v, want exactly one", dirs)
	}
	return dirs[0]
}

func TestProbeFullSequence(t *testing.T) {
	cam := newFakeCamera(t)
	o := testOptions(t)
	o.endpoint = cam.srv.URL + "/sony/camera"
	o.record, o.zoom = true, true

	if err := run(t.Context(), o); err != nil {
		t.Fatal(err)
	}
	dir := runDir(t, o)
	summary, err := os.ReadFile(filepath.Join(dir, "summary.txt"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(summary)
	for _, want := range []string{
		"Smart Remote Control", // application info
		"getEvent version 1.1", // highest getEvent version from getMethodTypes
		"camera ready",         // startRecMode wait ended on IDLE with rec-mode APIs
		"IDLE -> MovieRecording -> IDLE",
		"zoomPosition 0 -> in 50 -> out 10",
		"getAvailableFNumber",
		"64x48",
		"fps",
		"size M",
		"getSupportedFNumber: camera error 12 (No Such Method)", // failure recorded, probe continued
	} {
		if !strings.Contains(s, want) {
			t.Errorf("summary lacks %q:\n%s", want, s)
		}
	}

	for _, pattern := range []string{"*-getVersions.request.json", "*-getVersions.response.json", "*-startMovieRec.response.json", "*default-frame-1.jpg", "*default.stream.bin"} {
		if m, _ := filepath.Glob(filepath.Join(dir, pattern)); len(m) == 0 {
			t.Errorf("no file matching %s", pattern)
		}
	}
	req, _ := filepath.Glob(filepath.Join(dir, "*-getVersions.request.json"))
	if b, _ := os.ReadFile(req[0]); string(b) != `{"method":"getVersions","params":[],"id":1,"version":"1.0"}` {
		t.Errorf("raw request not saved as sent: %s", b)
	}

	cam.mu.Lock()
	defer cam.mu.Unlock()
	order := []string{"startRecMode@1.0", "setShootMode@1.0", "startMovieRec@1.0", "stopMovieRec@1.0", "actZoom@1.0", "startLiveview@1.0", "stopLiveview@1.0"}
	last := -1
	for _, m := range order {
		i := slices.Index(cam.calls, m)
		if i <= last {
			t.Errorf("%s at %d, out of order (calls %v)", m, i, cam.calls)
		}
		last = i
	}
	if slices.Contains(cam.calls, "getEvent@1.0") {
		t.Errorf("getEvent called at 1.0 though 1.1 is supported")
	}
}

func TestProbeWithoutRecordOrZoomLeavesThemAlone(t *testing.T) {
	cam := newFakeCamera(t)
	o := testOptions(t)
	o.endpoint = cam.srv.URL + "/sony/camera"
	if err := run(t.Context(), o); err != nil {
		t.Fatal(err)
	}
	cam.mu.Lock()
	defer cam.mu.Unlock()
	for _, m := range []string{"startMovieRec@1.0", "actZoom@1.0"} {
		if slices.Contains(cam.calls, m) {
			t.Errorf("%s called without its flag", m)
		}
	}
}

func TestProbeDiscoversEndpointViaSSDP(t *testing.T) {
	cam := newFakeCamera(t)
	dd := fmt.Sprintf(`<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0"><device><friendlyName>FAKE-RX</friendlyName><modelName>SonyImagingDevice</modelName><UDN>uuid:x</UDN>
<av:X_ScalarWebAPI_DeviceInfo xmlns:av="urn:schemas-sony-com:av"><av:X_ScalarWebAPI_Version>1.0</av:X_ScalarWebAPI_Version><av:X_ScalarWebAPI_ServiceList>
<av:X_ScalarWebAPI_Service><av:X_ScalarWebAPI_ServiceType>camera</av:X_ScalarWebAPI_ServiceType><av:X_ScalarWebAPI_ActionList_URL>%s/sony</av:X_ScalarWebAPI_ActionList_URL></av:X_ScalarWebAPI_Service>
</av:X_ScalarWebAPI_ServiceList></av:X_ScalarWebAPI_DeviceInfo></device></root>`, cam.srv.URL)
	ddSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, dd) }))
	defer ddSrv.Close()

	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			_, from, err := udp.ReadFrom(buf)
			if err != nil {
				return
			}
			udp.WriteTo([]byte("HTTP/1.1 200 OK\r\nLOCATION: "+ddSrv.URL+"/dd.xml\r\nST: urn:schemas-sony-com:service:ScalarWebAPI:1\r\n\r\n"), from)
		}
	}()

	o := testOptions(t)
	o.ssdp = true
	o.ssdpAddr = udp.LocalAddr().String()
	o.ssdpListen = 300 * time.Millisecond
	o.endpoint = "http://127.0.0.1:1/sony/camera" // must not be used
	if err := run(t.Context(), o); err != nil {
		t.Fatal(err)
	}
	dir := runDir(t, o)
	summary, _ := os.ReadFile(filepath.Join(dir, "summary.txt"))
	if !strings.Contains(string(summary), "endpoint "+cam.srv.URL+"/sony/camera (from device description)") {
		t.Errorf("endpoint not taken from the device description:\n%s", summary)
	}
	for _, pattern := range []string{"*-ssdp-request.txt", "*-ssdp-reply-1.txt", "*-device-description.xml"} {
		if m, _ := filepath.Glob(filepath.Join(dir, pattern)); len(m) == 0 {
			t.Errorf("no file matching %s", pattern)
		}
	}
}

func TestProbeFallsBackToEndpointWhenSSDPSilent(t *testing.T) {
	cam := newFakeCamera(t)
	silent, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	o := testOptions(t)
	o.ssdp = true
	o.ssdpAddr = silent.LocalAddr().String()
	o.ssdpListen = 100 * time.Millisecond
	o.endpoint = cam.srv.URL + "/sony/camera"
	if err := run(t.Context(), o); err != nil {
		t.Fatal(err)
	}
	summary, _ := os.ReadFile(filepath.Join(runDir(t, o), "summary.txt"))
	if !strings.Contains(string(summary), "endpoint "+o.endpoint+" (-endpoint fallback)") {
		t.Errorf("fallback endpoint not used:\n%s", summary)
	}
}

// The RX10M4 and RX100M6 both serve the camera service on port 10000.
func TestDefaultEndpointIsMeasuredPort(t *testing.T) {
	if got, want := defaultOptions().endpoint, "http://192.168.122.1:10000/sony/camera"; got != want {
		t.Errorf("default endpoint %q, want %q", got, want)
	}
}
