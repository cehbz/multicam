package console

import (
	"bytes"
	"context"
	"errors"
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

// named returns the camera at endpoint under name.
func named(t *testing.T, name, endpoint string) Named {
	t.Helper()
	cam, err := sony.NewCamera(endpoint, "")
	if err != nil {
		t.Fatal(err)
	}
	return Named{Name: name, Camera: Adapt(cam)}
}

// newConsole returns a fake camera and the console for it alone.
func newConsole(t *testing.T) (*sonytest.Camera, http.Handler) {
	fake := sonytest.NewCamera(t)
	return fake, New([]Named{named(t, "cam", fake.Endpoint())})
}

// twoCameras returns two fake cameras and the console for them as "front" and
// "side", in that order. The side camera's frames are its own.
func twoCameras(t *testing.T) (front, side *sonytest.Camera, console http.Handler) {
	front, side = sonytest.NewCamera(t), sonytest.NewCamera(t)
	side.Frames = [][]byte{[]byte("side frame 1"), []byte("side frame 2")}
	return front, side, New([]Named{named(t, "front", front.Endpoint()), named(t, "side", side.Endpoint())})
}

// get answers a GET of target from the console.
func get(console http.Handler, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	console.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

// srcs lists the page's sources matching re, in page order.
func srcs(re *regexp.Regexp, page string) []string {
	var l []string
	for _, m := range re.FindAllStringSubmatch(page, -1) {
		l = append(l, m[1])
	}
	return l
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

func TestPageEmbedsTheControl(t *testing.T) {
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

func TestPageShowsEveryCameraInOrder(t *testing.T) {
	_, _, console := twoCameras(t)
	rec := get(console, "/")
	page := rec.Body.String()
	if rec.Code != http.StatusOK || strings.Contains(page, "<script") {
		t.Fatalf("page: status %d, or it has a script: %q", rec.Code, page)
	}
	if got, want := srcs(imgSrc, page), []string{"/front/liveview", "/side/liveview"}; !slices.Equal(got, want) {
		t.Errorf("pictures %v, want %v", got, want)
	}
	if got, want := srcs(iframeSrc, page), []string{"/front/record", "/side/record"}; !slices.Equal(got, want) {
		t.Errorf("record controls %v, want %v", got, want)
	}
	// Each name comes before its camera's picture, and the first camera's
	// control before the second's name.
	last := -1
	for _, want := range []string{">front<", `src="/front/liveview"`, `src="/front/record"`, ">side<", `src="/side/liveview"`, `src="/side/record"`} {
		i := strings.Index(page, want)
		if i <= last {
			t.Errorf("page lacks %s after byte %d: %q", want, last, page)
		}
		last = max(last, i)
	}
}

func TestEachPictureRelaysItsOwnCamera(t *testing.T) {
	front, side, console := twoCameras(t)
	srv := httptest.NewServer(console)
	defer srv.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	for _, c := range []struct {
		path string
		fake *sonytest.Camera
	}{{"/side/liveview", side}, {"/front/liveview", front}} {
		resp, boundary := openStream(t, ctx, srv.URL+c.path)
		parts := multipart.NewReader(resp.Body, boundary)
		for i := range len(c.fake.Frames) + 1 {
			p, err := parts.NextPart()
			if err != nil {
				t.Fatalf("%s part %d: %v", c.path, i, err)
			}
			if got, _ := io.ReadAll(p); !bytes.Equal(got, c.fake.Frames[i%len(c.fake.Frames)]) {
				t.Fatalf("%s part %d is not its camera's frame %d: %.20q", c.path, i, i%len(c.fake.Frames), got)
			}
		}
	}
	for name, fake := range map[string]*sonytest.Camera{"front": front, "side": side} {
		if got, want := fake.Calls(), []string{"startLiveview@1.0"}; !slices.Equal(got, want) {
			t.Errorf("%s camera's calls %v, want %v", name, got, want)
		}
	}
}

func TestStartOnOneCameraLeavesTheOtherAlone(t *testing.T) {
	front, side, console := twoCameras(t)
	doc := press(t, console, control(t, console, "/side/record"))
	wantControl(t, "side after Start", doc, "recording", "Stop")
	wantControl(t, "front after side's Start", control(t, console, "/front/record"), "idle", "Start")
	if got, want := side.Calls(), []string{"getEvent@1.3", "startMovieRec@1.0", "getEvent@1.3"}; !slices.Equal(got, want) {
		t.Errorf("side camera's calls %v, want %v", got, want)
	}
	if got, want := front.Calls(), []string{"getEvent@1.3"}; !slices.Equal(got, want) {
		t.Errorf("front camera's calls %v, want %v", got, want)
	}
}

func TestUnknownCameraIsNotFound(t *testing.T) {
	front, side, console := twoCameras(t)
	// A request that reached a camera's liveview would run until its context
	// ends.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	for _, r := range []struct{ method, target string }{
		{http.MethodGet, "/back/liveview"},
		{http.MethodGet, "/back/record"},
		{http.MethodPost, "/back/record/start"},
		{http.MethodPost, "/back/record/stop"},
	} {
		rec := httptest.NewRecorder()
		console.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, r.method, r.target, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s: status %d, want 404", r.method, r.target, rec.Code)
		}
	}
	if calls := append(front.Calls(), side.Calls()...); len(calls) != 0 {
		t.Errorf("a camera was called for an unknown name: %v", calls)
	}
}

func TestUnreachableCameraLeavesTheOtherWorking(t *testing.T) {
	front := sonytest.NewCamera(t)
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close() // its address now refuses connections
	console := New([]Named{named(t, "gone", gone.URL+"/sony/camera"), named(t, "front", front.Endpoint())})
	var logged bytes.Buffer
	defer log.SetOutput(log.Writer())
	log.SetOutput(&logged)

	page := get(console, "/").Body.String()
	if got, want := srcs(iframeSrc, page), []string{"/gone/record", "/front/record"}; !slices.Equal(got, want) {
		t.Fatalf("record controls %v, want %v", got, want)
	}
	doc := control(t, console, "/gone/record")
	if !strings.Contains(doc, "<p>getEvent: ") || len(buttons(doc)) != 0 {
		t.Errorf("unreachable camera's control lacks its error or offers %v: %q", buttons(doc), doc)
	}
	if rec := get(console, "/gone/liveview"); rec.Code != http.StatusBadGateway {
		t.Errorf("unreachable camera's picture: status %d, want %d", rec.Code, http.StatusBadGateway)
	}
	if !strings.Contains(logged.String(), "camera=gone") {
		t.Errorf("log does not name the camera: %q", logged.String())
	}

	doc = press(t, console, control(t, console, "/front/record"))
	wantControl(t, "front after Start", doc, "recording", "Stop")
	srv := httptest.NewServer(console)
	defer srv.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	resp, boundary := openStream(t, ctx, srv.URL+"/front/liveview")
	p, err := multipart.NewReader(resp.Body, boundary).NextPart()
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := io.ReadAll(p); !bytes.Equal(got, front.Frames[0]) {
		t.Errorf("front picture's first part is not its camera's first frame")
	}
}

// stills is a camera that is not a Sony body: each liveview session yields its
// frames once and then ends.
type stills struct {
	frames   [][]byte
	startErr error
	closed   int
}

type stillsSession struct {
	cam  *stills
	next int
}

func (c *stills) Liveview(context.Context) (*stillsSession, error) {
	if c.startErr != nil {
		return nil, c.startErr
	}
	return &stillsSession{cam: c}, nil
}

func (c *stills) StartRecording(context.Context) error    { return nil }
func (c *stills) StopRecording(context.Context) error     { return nil }
func (c *stills) Recording(context.Context) (bool, error) { return false, nil }

func (s *stillsSession) Next() ([]byte, error) {
	if s.next == len(s.cam.frames) {
		return nil, io.EOF
	}
	s.next++
	return s.cam.frames[s.next-1], nil
}

func (s *stillsSession) Close() error {
	s.cam.closed++
	return nil
}

func TestRelaysAnyCamerasLiveview(t *testing.T) {
	cam := &stills{frames: [][]byte{[]byte("screen 1"), []byte("screen 2"), []byte("screen 3")}}
	console := New([]Named{{Name: "phone", Camera: Adapt(cam)}})

	rec := get(console, "/phone/liveview")
	mediaType, params, err := mime.ParseMediaType(rec.Header().Get("Content-Type"))
	if rec.Code != http.StatusOK || err != nil || mediaType != "multipart/x-mixed-replace" {
		t.Fatalf("status %d, Content-Type %q; want 200 multipart/x-mixed-replace", rec.Code, rec.Header().Get("Content-Type"))
	}
	parts := multipart.NewReader(rec.Body, params["boundary"])
	for i, want := range cam.frames {
		p, err := parts.NextPart()
		if err != nil {
			t.Fatalf("part %d: %v", i, err)
		}
		if got, _ := io.ReadAll(p); !bytes.Equal(got, want) || p.Header.Get("Content-Type") != "image/jpeg" {
			t.Errorf("part %d = %q as %q, want %q as image/jpeg", i, got, p.Header.Get("Content-Type"), want)
		}
	}
	if cam.closed != 1 {
		t.Errorf("liveview session closed %d times, want once", cam.closed)
	}
}

func TestAdaptedFailedStartHasNoSession(t *testing.T) {
	cam := &stills{startErr: errors.New("screen off")}
	lv, err := Adapt(cam).Liveview(t.Context())
	if err == nil || lv != nil {
		t.Errorf("Liveview = %#v, %v; want no session and the error", lv, err)
	}
	console := New([]Named{{Name: "phone", Camera: Adapt(cam)}})
	if rec := get(console, "/phone/liveview"); rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "screen off") {
		t.Errorf("picture: status %d %q, want %d with the error", rec.Code, rec.Body, http.StatusBadGateway)
	}
}
