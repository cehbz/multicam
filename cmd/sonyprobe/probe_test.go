package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cehbz/multicam/internal/sony/sonytest"
)

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
	cam := sonytest.NewCamera(t)
	o := testOptions(t)
	o.endpoint = cam.Endpoint()
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

	calls := cam.Calls()
	order := []string{"startRecMode@1.0", "setShootMode@1.0", "startMovieRec@1.0", "stopMovieRec@1.0", "actZoom@1.0", "startLiveview@1.0", "stopLiveview@1.0"}
	last := -1
	for _, m := range order {
		i := slices.Index(calls, m)
		if i <= last {
			t.Errorf("%s at %d, out of order (calls %v)", m, i, calls)
		}
		last = i
	}
	if slices.Contains(calls, "getEvent@1.0") {
		t.Errorf("getEvent called at 1.0 though 1.1 is supported")
	}
}

func TestProbeWithoutRecordOrZoomLeavesThemAlone(t *testing.T) {
	cam := sonytest.NewCamera(t)
	o := testOptions(t)
	o.endpoint = cam.Endpoint()
	if err := run(t.Context(), o); err != nil {
		t.Fatal(err)
	}
	calls := cam.Calls()
	for _, m := range []string{"startMovieRec@1.0", "actZoom@1.0"} {
		if slices.Contains(calls, m) {
			t.Errorf("%s called without its flag", m)
		}
	}
}

func TestProbeDiscoversEndpointViaSSDP(t *testing.T) {
	cam := sonytest.NewCamera(t)
	dd := fmt.Sprintf(`<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0"><device><friendlyName>FAKE-RX</friendlyName><modelName>SonyImagingDevice</modelName><UDN>uuid:x</UDN>
<av:X_ScalarWebAPI_DeviceInfo xmlns:av="urn:schemas-sony-com:av"><av:X_ScalarWebAPI_Version>1.0</av:X_ScalarWebAPI_Version><av:X_ScalarWebAPI_ServiceList>
<av:X_ScalarWebAPI_Service><av:X_ScalarWebAPI_ServiceType>camera</av:X_ScalarWebAPI_ServiceType><av:X_ScalarWebAPI_ActionList_URL>%s</av:X_ScalarWebAPI_ActionList_URL></av:X_ScalarWebAPI_Service>
</av:X_ScalarWebAPI_ServiceList></av:X_ScalarWebAPI_DeviceInfo></device></root>`, strings.TrimSuffix(cam.Endpoint(), "/camera"))
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
	if !strings.Contains(string(summary), "endpoint "+cam.Endpoint()+" (from device description)") {
		t.Errorf("endpoint not taken from the device description:\n%s", summary)
	}
	for _, pattern := range []string{"*-ssdp-request.txt", "*-ssdp-reply-1.txt", "*-device-description.xml"} {
		if m, _ := filepath.Glob(filepath.Join(dir, pattern)); len(m) == 0 {
			t.Errorf("no file matching %s", pattern)
		}
	}
}

func TestProbeFallsBackToEndpointWhenSSDPSilent(t *testing.T) {
	cam := sonytest.NewCamera(t)
	silent, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	o := testOptions(t)
	o.ssdp = true
	o.ssdpAddr = silent.LocalAddr().String()
	o.ssdpListen = 100 * time.Millisecond
	o.endpoint = cam.Endpoint()
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
