package console

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cehbz/multicam/internal/pixel"
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

func TestCommandLeavesTheViewersLiveviewRunning(t *testing.T) {
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

	if got, want := summary(ask(t, console, http.MethodPost, "/cam/start")), "cam=recording"; got != want {
		t.Fatalf("start: report %q, want %q", got, want)
	}
	for range len(fake.Frames) + 1 {
		next()
	}
	if calls := fake.Calls(); slices.Contains(calls, "stopLiveview@1.0") || !slices.Contains(calls, "startMovieRec@1.0") {
		t.Errorf("camera calls %v: want startMovieRec and no stopLiveview", calls)
	}
}

var (
	tileCamera = regexp.MustCompile(`<button class="tile" data-camera="([^"]+)">`)
	allButton  = regexp.MustCompile(`<button id="[^"]+" aria-label="([^"]+)">`)
	script     = regexp.MustCompile(`(?s)<script>(.*)</script>`)
)

func TestPageTilesEveryCameraInOrder(t *testing.T) {
	_, _, console := twoCameras(t)
	rec := get(console, "/")
	page := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("page: status %d", rec.Code)
	}
	if got, want := srcs(tileCamera, page), []string{"front", "side"}; !slices.Equal(got, want) {
		t.Errorf("tiles %v, want %v", got, want)
	}
	if got, want := srcs(imgSrc, page), []string{"/front/liveview", "/side/liveview"}; !slices.Equal(got, want) {
		t.Errorf("pictures %v, want %v", got, want)
	}
	// Each tile holds its camera's picture and name, and ends before the next
	// tile begins.
	last := -1
	for _, want := range []string{
		`data-camera="front"`, `src="/front/liveview"`, ">front<", "</button>",
		`data-camera="side"`, `src="/side/liveview"`, ">side<", "</button>",
	} {
		i := strings.Index(page[last+1:], want)
		if i < 0 {
			t.Fatalf("page lacks %s after byte %d: %q", want, last, page)
		}
		last += 1 + i
	}
}

// tile matches a tile's opening tag: what its class says after "tile", and
// its camera.
var tile = regexp.MustCompile(`<button class="tile([^"]*)" data-camera="([^"]+)">`)

func TestPageMarksTheTilesShownOnTheirSide(t *testing.T) {
	cam := Adapt(&stills{})
	console := New([]Named{{Name: "level", Camera: cam}, {Name: "phone", Camera: cam, OnItsSide: true}})
	var got []string
	for _, m := range tile.FindAllStringSubmatch(get(console, "/").Body.String(), -1) {
		got = append(got, m[2]+m[1])
	}
	if want := []string{"level", "phone on-its-side"}; !slices.Equal(got, want) {
		t.Errorf("tiles %q, want %q", got, want)
	}
}

func TestPageHasTheAllButtonsAndTheScript(t *testing.T) {
	_, _, console := twoCameras(t)
	page := get(console, "/").Body.String()
	if got, want := srcs(allButton, page), []string{"Start all", "Stop all"}; !slices.Equal(got, want) {
		t.Errorf("buttons for every camera %v, want %v", got, want)
	}
	if i, first := strings.Index(page, `aria-label="Stop all"`), strings.Index(page, `class="tile"`); i < 0 || i > first {
		t.Errorf("the buttons for every camera are not above the tiles: %q", page)
	}
	m := script.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("page has no script: %q", page)
	}
	// The script's routes.
	for _, route := range []string{"'/status'", "'/start'", "'/stop'"} {
		if !strings.Contains(m[1], route) {
			t.Errorf("script lacks %s: %q", route, m[1])
		}
	}
	if strings.Contains(page, "<iframe") {
		t.Errorf("page has a frame: %q", page)
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

func TestUnknownCameraIsNotFound(t *testing.T) {
	front, side, console := twoCameras(t)
	// A request that reached a camera's liveview would run until its context
	// ends.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	for _, r := range []struct{ method, target string }{
		{http.MethodGet, "/back/liveview"},
		{http.MethodPost, "/back/start"},
		{http.MethodPost, "/back/stop"},
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
	if got, want := srcs(tileCamera, page), []string{"gone", "front"}; !slices.Equal(got, want) {
		t.Fatalf("tiles %v, want %v", got, want)
	}
	states := ask(t, console, http.MethodGet, "/status")
	if got, want := summary(states), "gone=?! front=idle"; got != want || !strings.HasPrefix(states[0].Error, "getEvent: ") {
		t.Errorf("report %q with the unreachable camera's error %q, want %q with its failed status read", got, states[0].Error, want)
	}
	if rec := get(console, "/gone/liveview"); rec.Code != http.StatusBadGateway {
		t.Errorf("unreachable camera's picture: status %d, want %d", rec.Code, http.StatusBadGateway)
	}
	if !strings.Contains(logged.String(), "camera=gone") {
		t.Errorf("log does not name the camera: %q", logged.String())
	}

	if got, want := summary(ask(t, console, http.MethodPost, "/front/start")), "front=recording"; got != want {
		t.Errorf("front's start: report %q, want %q", got, want)
	}
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

// fakePixel is a Pixel behind a fake adb runner, with Pixel Camera in front:
// the shutter key toggles the recording its status reports.
type fakePixel struct {
	mu        sync.Mutex
	recording bool
	commands  []string // "status", "front" or "shutter"
}

func (p *fakePixel) run(_ context.Context, args ...string) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch command := strings.Join(args, " "); {
	case strings.HasPrefix(command, "shell dumpsys audio "):
		p.commands = append(p.commands, "status")
		if p.recording {
			return []byte("  session:20857 -- source client=CAMCORDER, dev=2ch 48000Hz ENCODING_PCM_16BIT -- uid:10171 -- patch:2329 -- pack:com.google.android.GoogleCamera -- format client=2ch 48000Hz ENCODING_PCM_16BIT, dev=2ch 48000Hz ENCODING_PCM_16BIT\n"), nil
		}
		return nil, nil
	case strings.HasPrefix(command, "shell dumpsys activity "):
		p.commands = append(p.commands, "front")
		return []byte("topResumedActivity=ActivityRecord{142494291 u0 com.google.android.GoogleCamera/com.google.android.apps.camera.activity.main.CameraActivity t53927}\n"), nil
	case command == "shell input keyevent 24":
		p.commands = append(p.commands, "shutter")
		p.recording = !p.recording
		return nil, nil
	default:
		return nil, fmt.Errorf("fake pixel: unexpected adb command %q", command)
	}
}

func (p *fakePixel) sent() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.commands)
}

// mixedRig is the console for three cameras in this order: "front", a Sony
// body that is recording; "side", a Sony body that is idle; and "phone", an
// idle Pixel. The Sony fakes' call logs start after the setup.
func mixedRig(t *testing.T) (front, side *sonytest.Camera, phone *fakePixel, console http.Handler, cameras []Named) {
	front, side, phone = sonytest.NewCamera(t), sonytest.NewCamera(t), &fakePixel{}
	cameras = []Named{
		named(t, "front", front.Endpoint()),
		named(t, "side", side.Endpoint()),
		{Name: "phone", Camera: Adapt(pixel.NewCamera(phone.run))},
	}
	front.SetCameraStatus("MovieRecording")
	return front, side, phone, New(cameras), cameras
}

// ask sends method to target and decodes the report it answers with.
func ask(t *testing.T, console http.Handler, method, target string) []state {
	t.Helper()
	rec := httptest.NewRecorder()
	console.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	if ct := rec.Header().Get("Content-Type"); rec.Code != http.StatusOK || ct != "application/json" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%s %s: status %d, Content-Type %q, Cache-Control %q; want 200 application/json no-store: %s",
			method, target, rec.Code, ct, rec.Header().Get("Cache-Control"), rec.Body)
	}
	var r report
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("%s %s: %v: %s", method, target, err, rec.Body)
	}
	return r.Cameras
}

// summary renders a report one camera per word: its name, then "=recording",
// "=idle" or "=?", then "!" when it carries an error.
func summary(states []state) string {
	var words []string
	for _, s := range states {
		word := s.Name + "=?"
		if s.Recording != nil {
			word = s.Name + map[bool]string{true: "=recording", false: "=idle"}[*s.Recording]
		}
		if s.Error != "" {
			word += "!"
		}
		words = append(words, word)
	}
	return strings.Join(words, " ")
}

func TestStatusReportsEveryCamera(t *testing.T) {
	front, side, phone, _, cameras := mixedRig(t)
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close() // its address now refuses connections
	console := New(append(cameras, named(t, "gone", gone.URL+"/sony/camera")))

	states := ask(t, console, http.MethodGet, "/status")
	if got, want := summary(states), "front=recording side=idle phone=idle gone=?!"; got != want {
		t.Fatalf("report %q, want %q", got, want)
	}
	if !strings.HasPrefix(states[3].Error, "getEvent: ") {
		t.Errorf("unreachable camera's error %q, want its failed status read", states[3].Error)
	}
	if got, want := front.Calls(), []string{"getEvent@1.3"}; !slices.Equal(got, want) {
		t.Errorf("front camera's calls %v, want %v", got, want)
	}
	if got, want := side.Calls(), []string{"getEvent@1.3"}; !slices.Equal(got, want) {
		t.Errorf("side camera's calls %v, want %v", got, want)
	}
	if got, want := phone.sent(), []string{"status"}; !slices.Equal(got, want) {
		t.Errorf("phone's commands %v, want %v", got, want)
	}
}

func TestStartAllStartsTheIdleCameras(t *testing.T) {
	front, side, phone, console, _ := mixedRig(t)

	if got, want := summary(ask(t, console, http.MethodPost, "/start")), "front=recording side=recording phone=recording"; got != want {
		t.Errorf("report %q, want %q", got, want)
	}
	if got, want := front.Calls(), []string{"getEvent@1.3"}; !slices.Equal(got, want) {
		t.Errorf("recording camera's calls %v, want only its status read %v", got, want)
	}
	if got, want := side.Calls(), []string{"getEvent@1.3", "startMovieRec@1.0", "getEvent@1.3"}; !slices.Equal(got, want) {
		t.Errorf("idle Sony camera's calls %v, want %v", got, want)
	}
	if got, want := phone.sent(), []string{"status", "status", "front", "shutter", "status", "status"}; !slices.Equal(got, want) {
		t.Errorf("idle Pixel's commands %v, want %v", got, want)
	}
}

func TestStopAllStopsTheRecordingCameras(t *testing.T) {
	front, side, phone, console, cameras := mixedRig(t)
	front.SetCameraStatus("")
	for _, cam := range []Named{cameras[0], cameras[2]} {
		if err := cam.StartRecording(t.Context()); err != nil {
			t.Fatal(err)
		}
	}

	if got, want := summary(ask(t, console, http.MethodPost, "/stop")), "front=idle side=idle phone=idle"; got != want {
		t.Errorf("report %q, want %q", got, want)
	}
	if got, want := front.Calls(), []string{"startMovieRec@1.0", "getEvent@1.3", "stopMovieRec@1.0", "getEvent@1.3"}; !slices.Equal(got, want) {
		t.Errorf("recording Sony camera's calls %v, want %v", got, want)
	}
	if got, want := side.Calls(), []string{"getEvent@1.3"}; !slices.Equal(got, want) {
		t.Errorf("idle camera's calls %v, want only its status read %v", got, want)
	}
	if phone.recording {
		t.Errorf("the Pixel is still recording after %v", phone.sent())
	}
}

func TestOneCamerasRefusalIsItsOwnError(t *testing.T) {
	front, side, console := twoCameras(t)
	side.Fail("startMovieRec", 40401, "Camera Not Ready")
	var logged bytes.Buffer
	defer log.SetOutput(log.Writer())
	log.SetOutput(&logged)

	states := ask(t, console, http.MethodPost, "/start")
	if got, want := summary(states), "front=recording side=idle!"; got != want {
		t.Fatalf("report %q, want %q", got, want)
	}
	if want := "startMovieRec: camera error 40401 (Camera Not Ready)"; states[1].Error != want {
		t.Errorf("refusing camera's error %q, want %q", states[1].Error, want)
	}
	if !slices.Contains(front.Calls(), "startMovieRec@1.0") {
		t.Errorf("the other camera was not started: %v", front.Calls())
	}
	if !strings.Contains(logged.String(), "camera=side") || !strings.Contains(logged.String(), "40401") {
		t.Errorf("log lacks the refusal: %q", logged.String())
	}
}

func TestRefusedStopLeavesTheCameraRecording(t *testing.T) {
	fake, console := newConsole(t)
	fake.Fail("stopMovieRec", 40401, "Not <b>Ready</b>")
	var logged bytes.Buffer
	defer log.SetOutput(log.Writer())
	log.SetOutput(&logged)

	if got, want := summary(ask(t, console, http.MethodPost, "/cam/start")), "cam=recording"; got != want {
		t.Fatalf("start: report %q, want %q", got, want)
	}
	states := ask(t, console, http.MethodPost, "/cam/stop")
	if got, want := summary(states), "cam=recording!"; got != want {
		t.Fatalf("stop: report %q, want %q", got, want)
	}
	if want := "stopMovieRec: camera error 40401 (Camera Not Ready): Not <b>Ready</b>"; states[0].Error != want {
		t.Errorf("refusing camera's error %q, want %q", states[0].Error, want)
	}
	if !strings.Contains(logged.String(), "camera=cam") || !strings.Contains(logged.String(), "40401") {
		t.Errorf("log lacks the refusal: %q", logged.String())
	}
}

func TestUnreachableCameraDoesNotStopTheOthers(t *testing.T) {
	front := sonytest.NewCamera(t)
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	console := New([]Named{named(t, "gone", gone.URL+"/sony/camera"), named(t, "front", front.Endpoint())})

	for _, c := range []struct{ target, want string }{
		{"/start", "gone=?! front=recording"},
		{"/stop", "gone=?! front=idle"},
	} {
		if got := summary(ask(t, console, http.MethodPost, c.target)); got != c.want {
			t.Errorf("POST %s: report %q, want %q", c.target, got, c.want)
		}
	}
}

func TestOneCamerasCommand(t *testing.T) {
	front, side, console := twoCameras(t)
	if got, want := summary(ask(t, console, http.MethodPost, "/side/start")), "side=recording"; got != want {
		t.Errorf("start: report %q, want %q", got, want)
	}
	if got, want := summary(ask(t, console, http.MethodPost, "/side/start")), "side=recording"; got != want {
		t.Errorf("start again: report %q, want %q", got, want)
	}
	if got, want := summary(ask(t, console, http.MethodPost, "/side/stop")), "side=idle"; got != want {
		t.Errorf("stop: report %q, want %q", got, want)
	}
	want := []string{"getEvent@1.3", "startMovieRec@1.0", "getEvent@1.3", "getEvent@1.3", "getEvent@1.3", "stopMovieRec@1.0", "getEvent@1.3"}
	if got := side.Calls(); !slices.Equal(got, want) {
		t.Errorf("side camera's calls %v, want %v", got, want)
	}
	if got := front.Calls(); len(got) != 0 {
		t.Errorf("the other camera was called: %v", got)
	}
	for _, target := range []string{"/back/start", "/back/stop"} {
		rec := httptest.NewRecorder()
		console.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, target, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("POST %s: status %d, want 404", target, rec.Code)
		}
	}
}

// timed is a camera whose every answer takes its own time: the time a real
// camera takes to answer, or to time out when err is set.
type timed struct {
	takes     time.Duration
	err       error
	recording bool
	startedAt time.Time // when StartRecording was called
}

func (c *timed) wait() error {
	time.Sleep(c.takes)
	return c.err
}

func (c *timed) Liveview(context.Context) (Liveview, error) { return nil, errors.New("no picture") }
func (c *timed) Recording(context.Context) (bool, error)    { return c.recording, c.wait() }
func (c *timed) StopRecording(context.Context) error        { return c.wait() }
func (c *timed) StartRecording(context.Context) error {
	c.startedAt = time.Now()
	if err := c.wait(); err != nil {
		return err
	}
	c.recording = true
	return nil
}

// Each camera is asked at the same moment, so the cameras' commands overlap
// and a report takes as long as its slowest camera, not the sum of them.
func TestCamerasAreAskedTogether(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		asleep := &timed{takes: 15 * time.Second, err: errors.New("timed out")}
		slow := &timed{takes: 2 * time.Second}
		quick := &timed{}
		console := New([]Named{{Name: "asleep", Camera: asleep}, {Name: "slow", Camera: slow}, {Name: "quick", Camera: quick}})

		began := time.Now()
		if got, want := summary(ask(t, console, http.MethodGet, "/status")), "asleep=?! slow=idle quick=idle"; got != want {
			t.Errorf("status report %q, want %q", got, want)
		}
		if took := time.Since(began); took != 15*time.Second {
			t.Errorf("status report took %v, want the slowest camera's 15s", took)
		}

		began = time.Now()
		if got, want := summary(ask(t, console, http.MethodPost, "/start")), "asleep=?! slow=recording quick=recording"; got != want {
			t.Errorf("start report %q, want %q", got, want)
		}
		if took := time.Since(began); took != 15*time.Second {
			t.Errorf("start took %v, want the slowest camera's 15s", took)
		}
		if at := quick.startedAt.Sub(began); at != 0 {
			t.Errorf("the quick camera was started %v into the command, want at once", at)
		}
		// The slow camera's status read and the quick camera's start were in
		// flight together.
		if at := slow.startedAt.Sub(began); at != 2*time.Second {
			t.Errorf("the slow camera was started %v into the command, want after its 2s status read", at)
		}
	})
}
