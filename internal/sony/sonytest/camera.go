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
// each JPEG packet followed by a frame-info packet. A long-polling getEvent
// blocks until an answer is scripted with Push or PushError, or the client
// goes away.
type Camera struct {
	// Frames are the liveview JPEG images: three distinct 64x48 images.
	Frames [][]byte

	t            *testing.T
	srv          *httptest.Server
	mu           sync.Mutex
	recMode      bool
	readyPolls   int
	shootMode    string
	recording    bool
	zoom         int
	calls        []string
	failures     map[string][]any
	status       string
	events       []event
	wake         chan struct{} // closed when an event is pushed
	busy         map[string]time.Duration
	inFlight     int
	mostInFlight int
}

// event is one scripted long-poll answer.
type event struct {
	status string // the cameraStatus, or none when empty
	err    []any  // a camera error in place of a result
}

// NewCamera starts a fake camera that is shut down when the test ends.
func NewCamera(t *testing.T) *Camera {
	c := &Camera{t: t, shootMode: "still", failures: map[string][]any{}, wake: make(chan struct{}), busy: map[string]time.Duration{}}
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

// Calls lists the RPCs received so far, in order, as "method@version", with
// a "+" after a long-polling getEvent.
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

// SetCameraStatus makes getEvent report status as the cameraStatus.
func (c *Camera) SetCameraStatus(status string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status = status
}

// Push scripts the next long-polling getEvent's answer: status as the
// cameraStatus, which later reads also report, or with an empty status an
// answer without a cameraStatus element.
func (c *Camera) Push(status string) { c.push(event{status: status}) }

// PushError scripts the next long-polling getEvent to answer with a camera
// error.
func (c *Camera) PushError(code int, message string) { c.push(event{err: []any{code, message}}) }

func (c *Camera) push(e event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
	close(c.wake)
	c.wake = make(chan struct{})
}

// Busy makes method take d to answer. MostInFlight counts busy calls.
func (c *Camera) Busy(method string, d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.busy[method] = d
}

// MostInFlight is the most busy calls that were in flight at once.
func (c *Camera) MostInFlight() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mostInFlight
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
	call := req.Method + "@" + req.Version
	longPolling := false
	if req.Method == "getEvent" {
		if len(req.Params) != 1 {
			c.t.Errorf("getEvent params %v, want [bool]", req.Params)
		} else if lp, ok := req.Params[0].(bool); !ok {
			c.t.Errorf("getEvent params %v, want [bool]", req.Params)
		} else if longPolling = lp; lp {
			call += "+"
		}
	}
	c.calls = append(c.calls, call)
	reply := func(key string, v any) {
		json.NewEncoder(w).Encode(map[string]any{key: v, "id": req.ID})
	}
	if d := c.busy[req.Method]; d > 0 {
		c.inFlight++
		c.mostInFlight = max(c.mostInFlight, c.inFlight)
		c.mu.Unlock()
		time.Sleep(d)
		c.mu.Lock()
		c.inFlight--
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
		if longPolling {
			for len(c.events) == 0 {
				wake := c.wake
				c.mu.Unlock()
				select {
				case <-wake:
				case <-r.Context().Done():
					c.mu.Lock()
					return
				}
				c.mu.Lock()
			}
			e := c.events[0]
			c.events = c.events[1:]
			if e.err != nil {
				reply("error", e.err)
				return
			}
			if e.status == "" {
				reply("result", []any{map[string]any{"type": "zoomInformation", "zoomPosition": c.zoom}})
				return
			}
			c.status = e.status
		}
		status := "IDLE"
		switch {
		case c.recMode && c.readyPolls > 0:
			status = "NotReady"
		case c.recording:
			status = "MovieRecording"
		}
		if c.status != "" {
			status = c.status
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
