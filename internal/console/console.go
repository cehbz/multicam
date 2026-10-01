// Package console serves the rig's browser console.
package console

import (
	"context"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/textproto"

	"github.com/cehbz/multicam/internal/sony"
)

const page = `<!doctype html>
<title>multicam</title>
<img src="/liveview" alt="camera liveview">
`

// New returns the console's handler: the page at / and the camera's liveview
// as MJPEG at /liveview. liveview starts a liveview session on the camera;
// each viewer gets its own, closed when the viewer's request ends.
func New(liveview func(context.Context) (*sony.Liveview, error)) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, page)
	})
	mux.HandleFunc("GET /liveview", func(w http.ResponseWriter, r *http.Request) {
		lv, err := liveview(r.Context())
		if err != nil {
			slog.Error("liveview", "err", err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer lv.Close()
		relay(w, lv)
	})
	return mux
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
