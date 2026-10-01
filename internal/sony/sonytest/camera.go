// Package sonytest provides a fake Camera Remote API body for tests.
package sonytest

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"
)

// Camera simulates a legacy-API body: the rec-mode API list appears only
// after startRecMode and a NotReady interval, startMovieRec only in movie mode.
// Its liveview stream repeats Frames in order until the client disconnects,
// each JPEG packet followed by a frame-info packet.
type Camera struct {
	// Frames are the liveview JPEG images: three distinct 64x48 images.
	Frames [][]byte

	t          *testing.T
	srv        *httptest.Server
	mu         sync.Mutex
	recMode    bool
	readyPolls int
	shootMode  string
	recording  bool
	zoom       int
	calls      []string
	failures   map[string][]any
}

// NewCamera starts a fake camera that is shut down when the test ends.
func NewCamera(t *testing.T) *Camera {
	c := &Camera{t: t, shootMode: "still", failures: map[string][]any{}}
	for _, shade := range []byte{0, 128, 255} {
		img := image.NewGray(image.Rect(0, 0, 64, 48))
		for i := range img.Pix {
			img.Pix[i] = shade
		}
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, img, nil); err != nil {
			t.Fatal(err)
		}
		c.Frames = append(c.Frames, buf.Bytes())
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/sony/camera", c.rpc)
	mux.HandleFunc("/liveview/liveviewstream", c.liveview)
	c.srv = httptest.NewServer(mux)
	t.Cleanup(c.srv.Close)
	return c
}

// Endpoint is the camera service URL.
func (c *Camera) Endpoint() string { return c.srv.URL + "/sony/camera" }

// Calls lists the RPCs received so far, in order, as "method@version".
func (c *Camera) Calls() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.calls)
}

// Fail makes every later call of method answer with a camera error.
func (c *Camera) Fail(method string, code int, message string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failures[method] = []any{code, message}
}

func (c *Camera) apis() []string {
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

func (c *Camera) rpc(w http.ResponseWriter, r *http.Request) {
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
	if e, ok := c.failures[req.Method]; ok {
		reply("error", e)
		return
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

// packet builds one liveview packet: the 8-byte common header, the 128-byte
// payload header with its type-specific bytes, the data and the padding.
func packet(typ byte, seq uint16, ts uint32, specific, data []byte, padding int) []byte {
	var p bytes.Buffer
	p.Write([]byte{0xFF, typ})
	binary.Write(&p, binary.BigEndian, seq)
	binary.Write(&p, binary.BigEndian, ts)
	hdr := make([]byte, 128)
	copy(hdr, []byte{0x24, 0x35, 0x68, 0x79})
	n := len(data)
	hdr[4], hdr[5], hdr[6], hdr[7] = byte(n>>16), byte(n>>8), byte(n), byte(padding)
	copy(hdr[8:], specific)
	p.Write(hdr)
	p.Write(data)
	p.Write(make([]byte, padding))
	return p.Bytes()
}

func (c *Camera) liveview(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/octet-stream")
	// One 16-byte frame-information record: version 1.0, count 1, size 16.
	infoHeader := []byte{1, 0, 0, 1, 0, 16}
	info := make([]byte, 16)
	seq := uint16(0)
	for i := 0; ; i++ {
		ts := uint32(i+1) * 40
		seq++
		p := packet(0x01, seq, ts, nil, c.Frames[i%len(c.Frames)], 3)
		seq++
		p = append(p, packet(0x02, seq, ts, infoHeader, info, 0)...)
		if _, err := w.Write(p); err != nil {
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
