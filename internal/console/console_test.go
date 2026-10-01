package console

import (
	"bytes"
	"context"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cehbz/multicam/internal/sony"
	"github.com/cehbz/multicam/internal/sony/sonytest"
)

// newConsole returns a fake camera and the console for it.
func newConsole(t *testing.T) (*sonytest.Camera, http.Handler) {
	fake := sonytest.NewCamera(t)
	return fake, New(sony.NewCamera(fake.Endpoint()))
}

var imgSrc = regexp.MustCompile(`<img src="([^"]+)"`)

// streamPath is the stream route as the page references it.
func streamPath(t *testing.T, console http.Handler) string {
	t.Helper()
	rec := httptest.NewRecorder()
	console.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	m := imgSrc.FindStringSubmatch(rec.Body.String())
	if m == nil {
		t.Fatalf("page has no <img src>: %q", rec.Body)
	}
	return m[1]
}

// openStream requests the stream route and returns the response and its
// multipart boundary.
func openStream(t *testing.T, ctx context.Context, url string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	mediaType, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if resp.StatusCode != http.StatusOK || err != nil || mediaType != "multipart/x-mixed-replace" || params["boundary"] == "" {
		t.Fatalf("stream response %s, Content-Type %q; want 200 multipart/x-mixed-replace with a boundary",
			resp.Status, resp.Header.Get("Content-Type"))
	}
	return resp, params["boundary"]
}

func TestPageShowsTheStream(t *testing.T) {
	_, console := newConsole(t)
	rec := httptest.NewRecorder()
	console.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if ct := rec.Header().Get("Content-Type"); rec.Code != http.StatusOK || !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("page: status %d, Content-Type %q; want 200 text/html", rec.Code, ct)
	}
	if strings.Contains(rec.Body.String(), "<script") {
		t.Errorf("page has a script: %q", rec.Body)
	}

	srv := httptest.NewServer(console)
	defer srv.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	openStream(t, ctx, srv.URL+streamPath(t, console))
}

func TestStreamRelaysFramesInOrder(t *testing.T) {
	fake, console := newConsole(t)
	srv := httptest.NewServer(console)
	defer srv.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	resp, boundary := openStream(t, ctx, srv.URL+streamPath(t, console))

	parts := multipart.NewReader(resp.Body, boundary)
	for i := range 2*len(fake.Frames) + 1 {
		p, err := parts.NextPart()
		if err != nil {
			t.Fatalf("part %d: %v", i, err)
		}
		if ct := p.Header.Get("Content-Type"); ct != "image/jpeg" {
			t.Errorf("part %d: Content-Type %q, want image/jpeg", i, ct)
		}
		got, err := io.ReadAll(p)
		if err != nil {
			t.Fatalf("part %d: %v", i, err)
		}
		if want := fake.Frames[i%len(fake.Frames)]; !bytes.Equal(got, want) {
			t.Fatalf("part %d: %d bytes, want the camera's frame %d unchanged (%d bytes)", i, len(got), i%len(fake.Frames), len(want))
		}
	}
}

// flushRecorder records the body length at each flush and ends the request
// after stopAfter flushes.
type flushRecorder struct {
	*httptest.ResponseRecorder
	flushedAt []int
	stopAfter int
	stop      context.CancelFunc
}

func (f *flushRecorder) Flush() {
	f.flushedAt = append(f.flushedAt, f.Body.Len())
	if len(f.flushedAt) == f.stopAfter {
		f.stop()
	}
}

func TestStreamFlushesEachFrame(t *testing.T) {
	fake, console := newConsole(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	rec := &flushRecorder{ResponseRecorder: httptest.NewRecorder(), stopAfter: len(fake.Frames) + 1, stop: cancel}
	console.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, streamPath(t, console), nil))

	if len(rec.flushedAt) != rec.stopAfter {
		t.Fatalf("%d flushes, want %d", len(rec.flushedAt), rec.stopAfter)
	}
	for i, n := range rec.flushedAt {
		if !bytes.HasSuffix(rec.Body.Bytes()[:n], fake.Frames[i%len(fake.Frames)]) {
			t.Errorf("flush %d at byte %d does not follow the camera's frame %d", i, n, i%len(fake.Frames))
		}
	}
}

func TestViewerDisconnectStopsLiveview(t *testing.T) {
	fake, console := newConsole(t)
	srv := httptest.NewServer(console)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	resp, boundary := openStream(t, ctx, srv.URL+streamPath(t, console))
	if _, err := multipart.NewReader(resp.Body, boundary).NextPart(); err != nil {
		t.Fatalf("first part: %v", err)
	}
	if got, want := fake.Calls(), []string{"startLiveview@1.0"}; !slices.Equal(got, want) {
		t.Fatalf("camera calls while viewing %v, want %v", got, want)
	}

	cancel()
	srv.Close() // returns once the stream handler has
	if got, want := fake.Calls(), []string{"startLiveview@1.0", "stopLiveview@1.0"}; !slices.Equal(got, want) {
		t.Errorf("camera calls after the viewer left %v, want %v", got, want)
	}
}

func TestStartErrorAnswersWithErrorStatus(t *testing.T) {
	fake, console := newConsole(t)
	fake.Fail("startLiveview", 40401, "Camera Not Ready")
	path := streamPath(t, console)
	var logged bytes.Buffer
	defer log.SetOutput(log.Writer())
	log.SetOutput(&logged)

	rec := httptest.NewRecorder()
	console.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status %d, want %d", rec.Code, http.StatusBadGateway)
	}
	if ct := rec.Header().Get("Content-Type"); strings.HasPrefix(ct, "multipart/") {
		t.Errorf("Content-Type %q on a failed start", ct)
	}
	if !strings.Contains(logged.String(), "40401") {
		t.Errorf("log %q lacks the camera error", logged.String())
	}
	if got, want := fake.Calls(), []string{"startLiveview@1.0"}; !slices.Equal(got, want) {
		t.Errorf("camera calls %v, want %v", got, want)
	}
}

var (
	iframeSrc  = regexp.MustCompile(`<iframe src="([^"]+)"`)
	formAction = regexp.MustCompile(`<form method="post" action="([^"]+)">`)
	button     = regexp.MustCompile(`<button>([^<]+)</button>`)
)

// controlPath is the record control's route as the page embeds it.
func controlPath(t *testing.T, console http.Handler) string {
	t.Helper()
	rec := httptest.NewRecorder()
	console.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	m := iframeSrc.FindStringSubmatch(rec.Body.String())
	if m == nil {
		t.Fatalf("page has no <iframe src>: %q", rec.Body)
	}
	return m[1]
}

// control fetches the record control document at target.
func control(t *testing.T, console http.Handler, target string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	console.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	if ct := rec.Header().Get("Content-Type"); rec.Code != http.StatusOK || !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("control %s: status %d, Content-Type %q; want 200 text/html", target, rec.Code, ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("control %s: Cache-Control %q, want no-store", target, cc)
	}
	if strings.Contains(rec.Body.String(), "<script") {
		t.Errorf("control has a script: %q", rec.Body)
	}
	return rec.Body.String()
}

// buttons lists the control's button labels.
func buttons(doc string) []string {
	var labels []string
	for _, m := range button.FindAllStringSubmatch(doc, -1) {
		labels = append(labels, m[1])
	}
	return labels
}

// press submits the control's one form as a browser would, and returns the
// document the browser ends on: the GET the command redirects to.
func press(t *testing.T, console http.Handler, doc string) string {
	t.Helper()
	forms := formAction.FindAllStringSubmatch(doc, -1)
	if len(forms) != 1 {
		t.Fatalf("control has %d forms, want 1: %q", len(forms), doc)
	}
	rec := httptest.NewRecorder()
	console.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, forms[0][1], nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST %s: status %d, want %d", forms[0][1], rec.Code, http.StatusSeeOther)
	}
	return control(t, console, rec.Header().Get("Location"))
}

// wantControl checks the status the control shows and its buttons.
func wantControl(t *testing.T, when, doc, status string, labels ...string) {
	t.Helper()
	if !strings.Contains(doc, status) || !slices.Equal(buttons(doc), labels) {
		t.Errorf("%s: control lacks %q or its buttons %v are not %v: %q", when, status, buttons(doc), labels, doc)
	}
}

func TestPageEmbedsTheControlBesideThePicture(t *testing.T) {
	_, console := newConsole(t)
	streamPath(t, console)
	doc := control(t, console, controlPath(t, console))
	if len(buttons(doc)) == 0 {
		t.Errorf("embedded control has no button: %q", doc)
	}
}

func TestControlFollowsStartAndStop(t *testing.T) {
	fake, console := newConsole(t)
	doc := control(t, console, controlPath(t, console))
	wantControl(t, "at first", doc, "idle", "Start")

	doc = press(t, console, doc)
	wantControl(t, "after Start", doc, "recording", "Stop")
	if calls := fake.Calls(); !slices.Contains(calls, "startMovieRec@1.0") {
		t.Errorf("camera calls after Start %v lack startMovieRec", calls)
	}

	doc = press(t, console, doc)
	wantControl(t, "after Stop", doc, "idle", "Start")
	if calls := fake.Calls(); !slices.Contains(calls, "stopMovieRec@1.0") {
		t.Errorf("camera calls after Stop %v lack stopMovieRec", calls)
	}
}

func TestRefusalShowsTheCameraError(t *testing.T) {
	// The camera's message reaches the document as text, not markup.
	const message, shown = "Not <b>Ready</b>", "camera error 40401 (Camera Not Ready): Not &lt;b&gt;Ready&lt;/b&gt;"
	var logged bytes.Buffer
	defer log.SetOutput(log.Writer())
	log.SetOutput(&logged)

	t.Run("start", func(t *testing.T) {
		fake, console := newConsole(t)
		fake.Fail("startMovieRec", 40401, message)
		doc := press(t, console, control(t, console, controlPath(t, console)))
		wantControl(t, "after a refused Start", doc, "idle", "Start")
		if !strings.Contains(doc, shown) {
			t.Errorf("control lacks %q: %q", shown, doc)
		}
	})
	t.Run("stop", func(t *testing.T) {
		fake, console := newConsole(t)
		fake.Fail("stopMovieRec", 40401, message)
		doc := press(t, console, control(t, console, controlPath(t, console)))
		doc = press(t, console, doc)
		wantControl(t, "after a refused Stop", doc, "recording", "Stop")
		if !strings.Contains(doc, shown) {
			t.Errorf("control lacks %q: %q", shown, doc)
		}
	})
	t.Run("status", func(t *testing.T) {
		fake, console := newConsole(t)
		fake.Fail("getEvent", 40401, message)
		path := controlPath(t, console)
		doc := control(t, console, path)
		if !strings.Contains(doc, shown) || !strings.Contains(doc, `<a href="`+path+`">`) {
			t.Errorf("control lacks %q or a link to redraw it: %q", shown, doc)
		}
		if b := buttons(doc); len(b) != 0 {
			t.Errorf("control offers %v without knowing the camera's status", b)
		}
	})
	if n := strings.Count(logged.String(), "40401"); n != 3 {
		t.Errorf("%d of the 3 refusals logged: %q", n, logged.String())
	}
}

func TestPressLeavesTheViewersLiveviewRunning(t *testing.T) {
	fake, console := newConsole(t)
	srv := httptest.NewServer(console)
	defer srv.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	resp, boundary := openStream(t, ctx, srv.URL+streamPath(t, console))
	parts := multipart.NewReader(resp.Body, boundary)
	frame := 0
	next := func() {
		t.Helper()
		p, err := parts.NextPart()
		if err != nil {
			t.Fatalf("part %d: %v", frame, err)
		}
		if got, _ := io.ReadAll(p); !bytes.Equal(got, fake.Frames[frame%len(fake.Frames)]) {
			t.Fatalf("part %d is not the camera's frame %d", frame, frame%len(fake.Frames))
		}
		frame++
	}
	next()

	doc := press(t, console, control(t, console, controlPath(t, console)))
	wantControl(t, "after Start", doc, "recording", "Stop")
	for range len(fake.Frames) + 1 {
		next()
	}
	if calls := fake.Calls(); slices.Contains(calls, "stopLiveview@1.0") || !slices.Contains(calls, "startMovieRec@1.0") {
		t.Errorf("camera calls %v: want startMovieRec and no stopLiveview", calls)
	}
}
