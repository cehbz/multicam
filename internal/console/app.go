package console

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"net/http"
	"text/template"
)

// The console as an installable web app: its manifest and icons, the script
// that registers its worker and keeps the screen on, and the worker, which
// carries the page shown when the server isn't running.
var (
	//go:embed app/manifest.webmanifest
	manifestJSON []byte
	//go:embed app/icon-192.png
	icon192 []byte
	//go:embed app/icon-512.png
	icon512 []byte
	//go:embed app/app.js
	appJS []byte
	//go:embed app/sw.js
	workerJS string
	//go:embed app/offline.html
	offlinePage string
)

// worker is the worker's script with the offline page in it, so a changed
// page is a changed worker and reaches the phone with the worker's update.
var worker = func() []byte {
	page, err := json.Marshal(offlinePage)
	if err != nil {
		panic(err)
	}
	var b bytes.Buffer
	template.Must(template.New("worker").Parse(workerJS)).Execute(&b, string(page))
	return b.Bytes()
}()

// serveApp adds the app's files to mux at the root, the worker's scope. Each
// is revalidated at every use, so a deploy reaches the phone at once.
func serveApp(mux *http.ServeMux) {
	for path, f := range map[string]struct {
		contentType string
		body        []byte
	}{
		"/manifest.webmanifest": {"application/manifest+json", manifestJSON},
		"/icon-192.png":         {"image/png", icon192},
		"/icon-512.png":         {"image/png", icon512},
		"/app.js":               {"text/javascript; charset=utf-8", appJS},
		"/sw.js":                {"text/javascript; charset=utf-8", worker},
	} {
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", f.contentType)
			w.Header().Set("Cache-Control", "no-cache")
			w.Write(f.body)
		})
	}
}
