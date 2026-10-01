// Package console serves the rig's browser console.
package console

import (
	"context"
	"html/template"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"

	"github.com/cehbz/multicam/internal/sony"
)

// Camera is what the console needs of a camera.
type Camera interface {
	Liveview(ctx context.Context) (*sony.Liveview, error)
	StartRecording(ctx context.Context) error
	StopRecording(ctx context.Context) error
	Recording(ctx context.Context) (bool, error)
}

// page shows the picture with the record control beside it, in a frame of its
// own so that a press redraws the control alone.
const page = `<!doctype html>
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>multicam</title>
<img src="/liveview" alt="camera liveview" style="max-width: 100%">
<iframe src="/record" title="record control" width="320" height="240"></iframe>
`

// controlDoc is the record control document: the errors of the last press and
// of the status read, the camera's status with the button that changes it, and
// a link that redraws the control.
var controlDoc = template.Must(template.New("control").Parse(`<!doctype html>
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>record</title>
<style>button { font-size: 2em; padding: .5em 1em }</style>
{{range .Errors}}<p>{{.}}</p>
{{end}}{{if eq .Status "recording"}}<form method="post" action="/record/stop">recording <button>Stop</button></form>
{{else if eq .Status "idle"}}<form method="post" action="/record/start">idle <button>Start</button></form>
{{end}}<p><a href="/record">refresh</a></p>
`))

// New returns the console's handler for cam: the page at /, the camera's
// liveview as MJPEG at /liveview and the record control at /record. Each
// viewer gets its own liveview session, closed when the viewer's request ends.
func New(cam Camera) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, page)
	})
	mux.HandleFunc("GET /liveview", func(w http.ResponseWriter, r *http.Request) {
		lv, err := cam.Liveview(r.Context())
		if err != nil {
			slog.Error("liveview", "err", err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer lv.Close()
		relay(w, lv)
	})
	mux.HandleFunc("GET /record", func(w http.ResponseWriter, r *http.Request) {
		// Status is "recording" or "idle", empty when the camera didn't say.
		var c struct {
			Errors []string
			Status string
		}
		if e := r.URL.Query().Get("error"); e != "" {
			c.Errors = append(c.Errors, e)
		}
		switch recording, err := cam.Recording(r.Context()); {
		case err != nil:
			slog.Error("record status", "err", err)
			c.Errors = append(c.Errors, err.Error())
		case recording:
			c.Status = "recording"
		default:
			c.Status = "idle"
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		controlDoc.Execute(w, c)
	})
	mux.Handle("POST /record/start", command(cam.StartRecording))
	mux.Handle("POST /record/stop", command(cam.StopRecording))
	return mux
}

// command runs one record command and redirects the browser to the control,
// with the command's error in the query when it failed.
func command(do func(context.Context) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		target := "/record"
		if err := do(r.Context()); err != nil {
			slog.Error("record", "err", err)
			target += "?" + url.Values{"error": {err.Error()}}.Encode()
		}
		http.Redirect(w, r, target, http.StatusSeeOther)
	}
}

// relay writes each JPEG frame of lv as one part of a multipart/x-mixed-replace
// response, flushed per frame, until lv or the viewer ends.
func relay(w http.ResponseWriter, lv *sony.Liveview) {
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
