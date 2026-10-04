// Package console serves the rig's browser console.
package console

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"sync"
	"time"
)

// whepPort is MediaMTX's WebRTC port, where the page plays streamed pictures
// from.
const whepPort = "8889"

// Liveview is a running stream of JPEG frames: Next yields the next frame,
// Close ends the stream.
type Liveview interface {
	Next() ([]byte, error)
	Close() error
}

// Source is a liveview source as its own package defines it, with sessions
// of its own type L.
type Source[L Liveview] interface {
	Liveview(ctx context.Context) (L, error)
}

// Relay returns the picture relayed from src.
func Relay[L Liveview](src Source[L]) Relayed { return Relayed{adapted[L]{src}} }

type adapted[L Liveview] struct{ Source[L] }

// Liveview starts the source's liveview. A failed start has no session.
func (a adapted[L]) Liveview(ctx context.Context) (Liveview, error) {
	lv, err := a.Source.Liveview(ctx)
	if err != nil {
		return nil, err
	}
	return lv, nil
}

// Picture is how a camera's picture reaches the page: Relayed or Streamed.
type Picture interface {
	// Stream is the MediaMTX path the page plays the picture from, empty
	// when the console relays its frames.
	Stream() string
}

// Relayed is a picture the console relays: its source's liveview frames as
// MJPEG at /{camera}/liveview.
type Relayed struct{ Source Source[Liveview] }

func (Relayed) Stream() string { return "" }

// Streamed is a picture the page plays from MediaMTX at Path over WHEP.
type Streamed struct{ Path string }

func (s Streamed) Stream() string { return s.Path }

// Camera is what the console needs of a camera.
type Camera interface {
	StartRecording(ctx context.Context) error
	StopRecording(ctx context.Context) error
	// Watch delivers whether the camera is recording: the current state
	// first, then each change, until the source ends and the channel closes.
	Watch(ctx context.Context) (<-chan bool, error)
}

// Named is a camera as the console shows it: under its name, which also keys
// its routes, with its picture, and its link when it has one of its own.
type Named struct {
	Name string
	Picture
	Camera
	Link Link // nil when the camera is reached without one
}

// connState is a camera's connection, or the server's: disconnected,
// connecting (a camera's try is running) or connected.
type connState string

const (
	disconnected connState = "disconnected"
	connecting   connState = "connecting"
	connected    connState = "connected"
)

// state is one camera's state as the page gets it: its connection, whether
// it is recording (known only while connected), and the error of its last
// try to connect or of a command.
type state struct {
	Name       string    `json:"name"`
	Connection connState `json:"connection"`
	Recording  *bool     `json:"recording,omitempty"`
	Error      string    `json:"error,omitempty"`
}

// same reports whether s and o are the same state.
func (s state) same(o state) bool {
	if s.Name != o.Name || s.Connection != o.Connection || s.Error != o.Error || (s.Recording == nil) != (o.Recording == nil) {
		return false
	}
	return s.Recording == nil || *s.Recording == *o.Recording
}

// recording reports whether s is known to be recording.
func (s state) recording() bool { return s.Recording != nil && *s.Recording }

// serverState is the server's connection as the page gets it: Connected or
// Disconnected, and whether a Connect is running.
type serverState struct {
	Connection connState `json:"connection"`
	Connecting bool      `json:"connecting"`
}

// report is the states of the cameras commanded, in the console's order.
type report struct {
	Cameras []state `json:"cameras"`
}

// watched is a camera with its last known state.
type watched struct {
	Named
	state state
	seq   uint64 // the console's seq when state was last set
}

// known is a camera's last known state and the seq it was set at.
type known struct {
	state
	seq uint64
}

// console is the cameras under their names, each with its last known state,
// the server's connection to them, and the viewers waiting for a change.
type console struct {
	cameras []*watched
	byName  map[string]*watched
	conn    *connection

	mu        sync.Mutex
	seq       uint64 // bumped at every change
	server    serverState
	serverSeq uint64 // the seq when server was last set
	subs      map[chan struct{}]struct{}
}

// newConsole returns the console for cameras, its connection restored from
// saved, which runs until ctx ends.
func newConsole(ctx context.Context, cameras []Named, saved StateFile) *console {
	c := &console{byName: map[string]*watched{}, subs: map[chan struct{}]struct{}{}, seq: 1,
		server: serverState{Connection: disconnected}, serverSeq: 1}
	for _, cam := range cameras {
		w := &watched{Named: cam, state: state{Name: cam.Name, Connection: disconnected}, seq: 1}
		c.cameras = append(c.cameras, w)
		c.byName[cam.Name] = w
	}
	c.conn = newConnection(ctx, c, saved)
	go c.conn.restore()
	return c
}

// set makes s w's last known state and wakes the viewers if it changed.
func (c *console) set(w *watched, s state) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if w.state.same(s) {
		return
	}
	c.seq++
	w.state, w.seq = s, c.seq
	c.wake()
}

// setServer makes s the server's state and wakes the viewers if it changed.
func (c *console) setServer(s serverState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.server == s {
		return
	}
	c.seq++
	c.server, c.serverSeq = s, c.seq
	c.wake()
}

// wake wakes every viewer; called with c.mu held.
func (c *console) wake() {
	for sub := range c.subs {
		select {
		case sub <- struct{}{}:
		default:
		}
	}
}

// last is w's last known state.
func (c *console) last(w *watched) state {
	c.mu.Lock()
	defer c.mu.Unlock()
	return w.state
}

// snapshot is every camera's last known state, in the console's order.
func (c *console) snapshot() []known {
	c.mu.Lock()
	defer c.mu.Unlock()
	states := make([]known, len(c.cameras))
	for i, w := range c.cameras {
		states[i] = known{w.state, w.seq}
	}
	return states
}

// serverSnapshot is the server's state and the seq it was set at.
func (c *console) serverSnapshot() (serverState, uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.server, c.serverSeq
}

// subscribe returns a channel woken at each change, and the call that ends
// the subscription.
func (c *console) subscribe() (<-chan struct{}, func()) {
	wake := make(chan struct{}, 1)
	c.mu.Lock()
	c.subs[wake] = struct{}{}
	c.mu.Unlock()
	return wake, func() {
		c.mu.Lock()
		delete(c.subs, wake)
		c.mu.Unlock()
	}
}

// record starts, or stops, every connected camera of cams whose last known
// state is not recording, or is, and reports their last known states
// afterwards. A camera not connected is not commanded and carries the error
// "not connected". All are commanded at the same moment and each answers for
// itself, so a report takes as long as its slowest camera. A camera that
// fails or refuses carries its error with the state it had.
func (c *console) record(ctx context.Context, cams []*watched, recording bool) report {
	name, command := "record stop", Camera.StopRecording
	if recording {
		name, command = "record start", Camera.StartRecording
	}
	states := make([]state, len(cams))
	var wg sync.WaitGroup
	for i, w := range cams {
		wg.Go(func() {
			s := c.last(w)
			if s.Connection != connected {
				s.Error = "not connected"
				states[i] = s
				return
			}
			if s.Recording != nil && *s.Recording == recording {
				states[i] = s
				return
			}
			err := command(w.Camera, ctx)
			s = c.last(w)
			if err != nil {
				slog.Error(name, "camera", w.Name, "err", err)
				s.Error = err.Error()
			}
			states[i] = s
		})
	}
	wg.Wait()
	return report{Cameras: states}
}

//go:embed page.html
var pageHTML string

// page tiles the cameras' pictures under the buttons that start and stop
// them all. Its script sends a tap's command and shows each camera's state
// as the console pushes it.
var page = template.Must(template.New("page").Parse(pageHTML))

// New returns the console's handler for cameras, whose connection runs until
// ctx ends and starts as saved says: the page at /, never cached, the files
// that install it as an app, and the liveview of the relayed camera a path
// names as MJPEG at /{camera}/liveview while the camera is connected. Each
// viewer of a picture gets its own liveview session, closed when the
// viewer's request or the camera's connection ends. For the page's script,
// GET /events pushes the server's connection and every camera's state and
// then each change as server-sent events; POST /connect and POST /disconnect
// connect and disconnect the server; POST /start and POST /stop start and
// stop every camera, and POST /{camera}/start and /{camera}/stop one.
func New(ctx context.Context, cameras []Named, saved StateFile) http.Handler {
	c := newConsole(ctx, cameras, saved)
	// named serves a route of the camera its path names; 404 when none has
	// the name.
	named := func(serve func(http.ResponseWriter, *http.Request, *watched)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			cam, ok := c.byName[r.PathValue("camera")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			serve(w, r, cam)
		}
	}

	mux := http.NewServeMux()
	serveApp(mux)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		page.Execute(w, struct {
			Cameras  []Named
			WHEPPort string
		}{cameras, whepPort})
	})
	mux.Handle("GET /{camera}/liveview", named(func(w http.ResponseWriter, r *http.Request, cam *watched) {
		relayed, ok := cam.Picture.(Relayed)
		if !ok {
			http.NotFound(w, r)
			return
		}
		live := c.conn.live(cam)
		if live == nil {
			http.Error(w, "not connected", http.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		defer context.AfterFunc(live, cancel)()
		lv, err := relayed.Source.Liveview(ctx)
		if err != nil {
			slog.Error("liveview", "camera", cam.Name, "err", err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer lv.Close()
		relay(w, lv)
	}))

	// The page's script: the cameras together at the root, one camera under
	// its name. Each command answers with a report as JSON.
	mux.HandleFunc("GET /events", c.events)
	mux.HandleFunc("POST /connect", func(w http.ResponseWriter, r *http.Request) {
		answer(w, c.conn.Connect(r.Context()))
	})
	mux.HandleFunc("POST /disconnect", func(w http.ResponseWriter, r *http.Request) {
		answer(w, c.conn.Disconnect())
	})
	mux.HandleFunc("POST /start", func(w http.ResponseWriter, r *http.Request) {
		answer(w, c.record(r.Context(), c.cameras, true))
	})
	mux.HandleFunc("POST /stop", func(w http.ResponseWriter, r *http.Request) {
		answer(w, c.record(r.Context(), c.cameras, false))
	})
	mux.Handle("POST /{camera}/start", named(func(w http.ResponseWriter, r *http.Request, cam *watched) {
		answer(w, c.record(r.Context(), []*watched{cam}, true))
	}))
	mux.Handle("POST /{camera}/stop", named(func(w http.ResponseWriter, r *http.Request, cam *watched) {
		answer(w, c.record(r.Context(), []*watched{cam}, false))
	}))
	return mux
}

// keepAlive is how often an idle event stream carries a comment.
const keepAlive = 15 * time.Second

// events streams the server's state as a "connection" event and every
// camera's last known state as an unnamed event, then each change, each
// event's data the state as JSON, until the viewer leaves. An idle stream
// carries a comment every keepAlive.
func (c *console) events(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	flusher := http.NewResponseController(w)
	wake, unsubscribe := c.subscribe()
	defer unsubscribe()
	ticker := time.NewTicker(keepAlive)
	defer ticker.Stop()
	sent := map[string]uint64{} // seq of each camera's state last sent
	var serverSent uint64
	for {
		if s, seq := c.serverSnapshot(); seq > serverSent {
			serverSent = seq
			data, _ := json.Marshal(s)
			fmt.Fprintf(w, "event: connection\ndata: %s\n\n", data)
		}
		for _, k := range c.snapshot() {
			if k.seq <= sent[k.Name] {
				continue
			}
			sent[k.Name] = k.seq
			data, _ := json.Marshal(k.state)
			fmt.Fprintf(w, "data: %s\n\n", data)
		}
		if err := flusher.Flush(); err != nil {
			return
		}
		select {
		case <-wake:
		case <-ticker.C:
			fmt.Fprint(w, ": keep-alive\n\n")
			if err := flusher.Flush(); err != nil {
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}

// answer writes a command's answer as JSON.
func answer(w http.ResponseWriter, r any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(r)
}

// relay writes each JPEG frame of lv as one part of a multipart/x-mixed-replace
// response, flushed per frame, until lv or the viewer ends.
func relay(w http.ResponseWriter, lv Liveview) {
	parts := multipart.NewWriter(w)
	w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary="+parts.Boundary())
	flusher := http.NewResponseController(w)
	for {
		jpeg, err := lv.Next()
		if err != nil {
			return
		}
		part, err := parts.CreatePart(textproto.MIMEHeader{"Content-Type": {"image/jpeg"}})
		if err != nil {
			return
		}
		if _, err := part.Write(jpeg); err != nil {
			return
		}
		if err := flusher.Flush(); err != nil {
			return
		}
	}
}
