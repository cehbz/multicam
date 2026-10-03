// Package console serves the rig's browser console.
package console

import (
	"context"
	_ "embed"
	"encoding/json"
	"html/template"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"sync"
)

// Liveview is a running stream of JPEG frames: Next yields the next frame,
// Close ends the stream.
type Liveview interface {
	Next() ([]byte, error)
	Close() error
}

// Camera is what the console needs of a camera.
type Camera interface {
	Liveview(ctx context.Context) (Liveview, error)
	StartRecording(ctx context.Context) error
	StopRecording(ctx context.Context) error
	Recording(ctx context.Context) (bool, error)
}

// Source is a camera as its own package defines it, with liveview sessions of
// its own type L.
type Source[L Liveview] interface {
	Liveview(ctx context.Context) (L, error)
	StartRecording(ctx context.Context) error
	StopRecording(ctx context.Context) error
	Recording(ctx context.Context) (bool, error)
}

// Adapt returns src as a Camera.
func Adapt[L Liveview](src Source[L]) Camera { return adapted[L]{src} }

type adapted[L Liveview] struct{ Source[L] }

// Liveview starts the source's liveview. A failed start has no session.
func (a adapted[L]) Liveview(ctx context.Context) (Liveview, error) {
	lv, err := a.Source.Liveview(ctx)
	if err != nil {
		return nil, err
	}
	return lv, nil
}

// Named is a camera as the console shows it: under its name, which also keys
// its routes.
type Named struct {
	Name string
	Camera
}

// state is one camera's line of a report: whether it is recording, absent
// when the camera didn't say, and the error of its status read or command.
type state struct {
	Name      string `json:"name"`
	Recording *bool  `json:"recording,omitempty"`
	Error     string `json:"error,omitempty"`
}

// report is the states of the cameras asked, in the console's order.
type report struct {
	Cameras []state `json:"cameras"`
}

// state reads the camera's recording status.
func (cam Named) state(ctx context.Context) state {
	recording, err := cam.Recording(ctx)
	if err != nil {
		slog.Error("record status", "camera", cam.Name, "err", err)
		return state{Name: cam.Name, Error: err.Error()}
	}
	return state{Name: cam.Name, Recording: &recording}
}

// group is cameras acted on together: all are asked at the same moment and
// each answers for itself, so a report takes as long as its slowest camera.
type group []Named

// each asks every camera at once and reports their answers.
func (g group) each(ask func(Named) state) report {
	states := make([]state, len(g))
	var wg sync.WaitGroup
	for i, cam := range g {
		wg.Go(func() { states[i] = ask(cam) })
	}
	wg.Wait()
	return report{Cameras: states}
}

// status reports every camera's recording status.
func (g group) status(ctx context.Context) report {
	return g.each(func(cam Named) state { return cam.state(ctx) })
}

// record starts every camera that isn't recording, or stops every one that
// is, and reports their statuses afterwards. A camera that fails or refuses
// carries its error with the status it had.
func (g group) record(ctx context.Context, recording bool) report {
	name, command := "record stop", Camera.StopRecording
	if recording {
		name, command = "record start", Camera.StartRecording
	}
	return g.each(func(cam Named) state {
		s := cam.state(ctx)
		if s.Recording == nil || *s.Recording == recording {
			return s
		}
		if err := command(cam, ctx); err != nil {
			slog.Error(name, "camera", cam.Name, "err", err)
			s.Error = err.Error()
			return s
		}
		return cam.state(ctx)
	})
}

//go:embed page.html
var pageHTML string

// page tiles the cameras' pictures under the buttons that start and stop
// them all. Its script sends a tap's command and keeps the tiles' statuses
// current.
var page = template.Must(template.New("page").Parse(pageHTML))

// New returns the console's handler for cameras: the page at / and the
// liveview of the camera a path names as MJPEG at /{camera}/liveview. Each
// viewer of a picture gets its own liveview session, closed when the viewer's
// request ends. For the page's script, GET /status reports every camera, POST
// /start and POST /stop start and stop them all, and POST /{camera}/start and
// /{camera}/stop one.
func New(cameras []Named) http.Handler {
	byName := map[string]Named{}
	for _, cam := range cameras {
		byName[cam.Name] = cam
	}
	// named serves a route of the camera its path names; 404 when none has
	// the name.
	named := func(serve func(http.ResponseWriter, *http.Request, Named)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			cam, ok := byName[r.PathValue("camera")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			serve(w, r, cam)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		page.Execute(w, cameras)
	})
	mux.Handle("GET /{camera}/liveview", named(func(w http.ResponseWriter, r *http.Request, cam Named) {
		lv, err := cam.Liveview(r.Context())
		if err != nil {
			slog.Error("liveview", "camera", cam.Name, "err", err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer lv.Close()
		relay(w, lv)
	}))

	// The page's script: the cameras together at the root, one camera under
	// its name. Each answers with a report as JSON.
	all := group(cameras)
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		answer(w, all.status(r.Context()))
	})
	mux.HandleFunc("POST /start", func(w http.ResponseWriter, r *http.Request) {
		answer(w, all.record(r.Context(), true))
	})
	mux.HandleFunc("POST /stop", func(w http.ResponseWriter, r *http.Request) {
		answer(w, all.record(r.Context(), false))
	})
	mux.Handle("POST /{camera}/start", named(func(w http.ResponseWriter, r *http.Request, cam Named) {
		answer(w, group{cam}.record(r.Context(), true))
	}))
	mux.Handle("POST /{camera}/stop", named(func(w http.ResponseWriter, r *http.Request, cam Named) {
		answer(w, group{cam}.record(r.Context(), false))
	}))
	return mux
}

// answer writes a report as JSON.
func answer(w http.ResponseWriter, r report) {
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
