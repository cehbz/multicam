// Package console serves the rig's browser console.
package console

import (
	"context"
	"html/template"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
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

// Named is a camera under the name the console shows it by and keys its
// routes with.
type Named struct {
	Name string
	Camera
}

// page shows each camera under its name: its picture, then its record control
// in a frame of its own, so that a press redraws that control alone.
var page = template.Must(template.New("page").Parse(`<!doctype html>
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>multicam</title>
{{range .}}<h2>{{.Name}}</h2>
<img src="/{{.Name}}/liveview" alt="{{.Name}} liveview" style="display: block; max-width: 100%">
<iframe src="/{{.Name}}/record" title="{{.Name}} record control" style="width: 100%" height="160"></iframe>
{{end}}`))

// controlDoc is one camera's record control document: the errors of the last
// press and of the status read, the camera's status with the button that
// changes it, and a link that redraws the control.
var controlDoc = template.Must(template.New("control").Parse(`<!doctype html>
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Name}} record</title>
<style>button { font-size: 2em; padding: .5em 1em }</style>
{{range .Errors}}<p>{{.}}</p>
{{end}}{{if eq .Status "recording"}}<form method="post" action="/{{.Name}}/record/stop">recording <button>Stop</button> <a href="/{{.Name}}/record">refresh</a></form>
{{else if eq .Status "idle"}}<form method="post" action="/{{.Name}}/record/start">idle <button>Start</button> <a href="/{{.Name}}/record">refresh</a></form>
{{else}}<p><a href="/{{.Name}}/record">refresh</a></p>
{{end}}`))

// New returns the console's handler for cameras: the page at / and, for the
// camera a path names, its liveview as MJPEG at /{camera}/liveview and its
// record control at /{camera}/record. Each viewer of a picture gets its own
// liveview session, closed when the viewer's request ends.
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
	// command runs one record command and redirects the browser to the
	// camera's control, with the command's error in the query when it failed.
	command := func(name string, do func(Camera, context.Context) error) http.HandlerFunc {
		return named(func(w http.ResponseWriter, r *http.Request, cam Named) {
			target := "/" + url.PathEscape(cam.Name) + "/record"
			if err := do(cam, r.Context()); err != nil {
				slog.Error(name, "camera", cam.Name, "err", err)
				target += "?" + url.Values{"error": {err.Error()}}.Encode()
			}
			http.Redirect(w, r, target, http.StatusSeeOther)
		})
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
	mux.Handle("GET /{camera}/record", named(func(w http.ResponseWriter, r *http.Request, cam Named) {
		// Status is "recording" or "idle", empty when the camera didn't say.
		c := struct {
			Name   string
			Errors []string
			Status string
		}{Name: cam.Name}
		if e := r.URL.Query().Get("error"); e != "" {
			c.Errors = append(c.Errors, e)
		}
		switch recording, err := cam.Recording(r.Context()); {
		case err != nil:
			slog.Error("record status", "camera", cam.Name, "err", err)
			c.Errors = append(c.Errors, err.Error())
		case recording:
			c.Status = "recording"
		default:
			c.Status = "idle"
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		controlDoc.Execute(w, c)
	}))
	mux.Handle("POST /{camera}/record/start", command("record start", Camera.StartRecording))
	mux.Handle("POST /{camera}/record/stop", command("record stop", Camera.StopRecording))
	return mux
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
