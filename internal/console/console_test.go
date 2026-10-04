package console

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cehbz/multicam/internal/sony"
	"github.com/cehbz/multicam/internal/sony/sonytest"
)

// feed is a camera whose recording state the test delivers: each Watch hands
// out a fresh channel, sent on watched, that the test feeds and closes. The
// watch ends, as a camera's does, when its context ends.
type feed struct {
	watched  chan chan bool
	watchErr atomic.Pointer[error] // Watch fails with it when set
	watches  atomic.Int32          // Watch calls
	startErr error
	stopErr  error
	starts   atomic.Int32
	stops    atomic.Int32

	mu   sync.Mutex
	ctxs []context.Context // each successful Watch's context
}

func newFeed() *feed { return &feed{watched: make(chan chan bool, 8)} }

func (f *feed) failWatch(err error) { f.watchErr.Store(&err) }

func (f *feed) Watch(ctx context.Context) (<-chan bool, error) {
	f.watches.Add(1)
	if err := f.watchErr.Load(); err != nil {
		return nil, *err
	}
	f.mu.Lock()
	f.ctxs = append(f.ctxs, ctx)
	f.mu.Unlock()
	in, out := make(chan bool), make(chan bool)
	f.watched <- in
	go func() {
		defer close(out)
		for {
			select {
			case v, ok := <-in:
				if !ok {
					return
				}
				select {
				case out <- v:
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// watching reports whether the feed's last watch is still running.
func (f *feed) watching() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.ctxs) > 0 && f.ctxs[len(f.ctxs)-1].Err() == nil
}

func (f *feed) StartRecording(context.Context) error {
	f.starts.Add(1)
	return f.startErr
}

func (f *feed) StopRecording(context.Context) error {
	f.stops.Add(1)
	return f.stopErr
}

// watch returns the channel of the feed's next Watch, failing after d; with
// no d, failing unless the Watch already happened.
func (f *feed) watch(t *testing.T, d time.Duration) chan bool {
	t.Helper()
	if d == 0 {
		select {
		case ch := <-f.watched:
			return ch
		default:
			t.Fatal("the camera was not watched")
			return nil
		}
	}
	select {
	case ch := <-f.watched:
		return ch
	case <-time.After(d):
		t.Fatal("the camera was not watched")
		return nil
	}
}

// deliver feeds the state and waits for the console to take it.
func (f *feed) deliver(t *testing.T, ch chan bool, recording bool) {
	t.Helper()
	select {
	case ch <- recording:
	case <-time.After(5 * time.Second):
		t.Fatal("the console did not take the state")
	}
}

// up waits for the console to watch the feed's camera and delivers its first
// state, idle, which connects it.
func up(t *testing.T, f *feed) chan bool {
	t.Helper()
	ch := f.watch(t, time.Second)
	f.deliver(t, ch, false)
	return ch
}

// awaitUp waits for the console to show each camera named connected.
func awaitUp(t *testing.T, console http.Handler, names ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	pr, pw := io.Pipe()
	go func() {
		console.ServeHTTP(&pipeWriter{header: http.Header{}, body: pw}, httptest.NewRequestWithContext(ctx, http.MethodGet, "/events", nil))
		pw.Close()
	}()
	defer pr.Close()
	events := make(chan string, 64)
	go func() {
		defer close(events)
		readEvents(bufio.NewScanner(pr), events)
	}()
	waiting := map[string]bool{}
	for _, name := range names {
		waiting[name] = true
	}
	for e := range events {
		var s state
		if json.Unmarshal([]byte(e), &s) == nil && s.Connection == connected {
			delete(waiting, s.Name)
		}
		if len(waiting) == 0 {
			return
		}
	}
	t.Fatalf("cameras %v not connected within 5 s", waiting)
}

// saved returns a state file holding the connection state, Connected or
// Disconnected.
func saved(t *testing.T, connected bool) StateFile {
	t.Helper()
	f := StateFile(filepath.Join(t.TempDir(), "connection.state"))
	if err := f.Save(connected); err != nil {
		t.Fatal(err)
	}
	return f
}

// relayed returns the Sony body at endpoint under name, its state fed by the
// returned feed.
func relayed(t *testing.T, name, endpoint string) (Named, *feed) {
	t.Helper()
	cam, err := sony.NewCamera(endpoint, "")
	if err != nil {
		t.Fatal(err)
	}
	f := newFeed()
	return Named{Name: name, Picture: Relay(cam), Camera: f}, f
}

// fed returns a camera under name whose picture is streamed, its state fed by
// the returned feed.
func fed(name string) (Named, *feed) {
	f := newFeed()
	return Named{Name: name, Picture: Streamed{Path: name}, Camera: f}, f
}

// oneBody returns a fake Sony body, its feed and the connected console for
// it alone.
func oneBody(t *testing.T) (*sonytest.Camera, *feed, http.Handler) {
	fake := sonytest.NewCamera(t)
	cam, f := relayed(t, "cam", fake.Endpoint())
	console := New(t.Context(), []Named{cam}, saved(t, true))
	up(t, f)
	awaitUp(t, console, "cam")
	return fake, f, console
}

// twoCameras returns two fake Sony bodies and the connected console for them
// as "front" and "side", in that order. The side camera's frames are its own.
func twoCameras(t *testing.T) (front, side *sonytest.Camera, console http.Handler) {
	front, side = sonytest.NewCamera(t), sonytest.NewCamera(t)
	side.Frames = [][]byte{[]byte("side frame 1"), []byte("side frame 2")}
	f, ff := relayed(t, "front", front.Endpoint())
	s, sf := relayed(t, "side", side.Endpoint())
	console = New(t.Context(), []Named{f, s}, saved(t, true))
	up(t, ff)
	up(t, sf)
	awaitUp(t, console, "front", "side")
	return front, side, console
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

var imgSrc = regexp.MustCompile(`<img data-liveview="([^"]+)"`)

// streamPath is the stream route as the page references it.
func streamPath(t *testing.T, console http.Handler) string {
	t.Helper()
	rec := httptest.NewRecorder()
	console.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	m := imgSrc.FindStringSubmatch(rec.Body.String())
	if m == nil {
		t.Fatalf("page has no <img data-liveview>: %q", rec.Body)
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
	_, _, console := oneBody(t)
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
	fake, _, console := oneBody(t)
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
	fake, _, console := oneBody(t)
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
	fake, _, console := oneBody(t)
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
	fake, _, console := oneBody(t)
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
	fake, f, console := oneBody(t)
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

	ask(t, console, http.MethodPost, "/cam/start")
	if f.starts.Load() != 1 {
		t.Fatalf("camera started %d times, want once", f.starts.Load())
	}
	for range len(fake.Frames) + 1 {
		next()
	}
	if calls := fake.Calls(); slices.Contains(calls, "stopLiveview@1.0") {
		t.Errorf("camera calls %v: want no stopLiveview", calls)
	}
}

var (
	tileCamera = regexp.MustCompile(`<button class="tile" data-camera="([^"]+)">`)
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
		`data-camera="front"`, `data-liveview="/front/liveview"`, ">front<", "</button>",
		`data-camera="side"`, `data-liveview="/side/liveview"`, ">side<", "</button>",
	} {
		i := strings.Index(page[last+1:], want)
		if i < 0 {
			t.Fatalf("page lacks %s after byte %d: %q", want, last, page)
		}
		last += 1 + i
	}
}

// barControl is a control of the bar: a button by its label, or the dot or
// the fault text by its id.
var barControl = regexp.MustCompile(`<button [^>]*aria-label="([^"]+)"|<button id="(connect|disconnect)"|<[a-z]+ id="(dot|fault)"`)

func TestPageBarHasTheMenuTheDotTheAllButtonsAndReload(t *testing.T) {
	_, _, console := twoCameras(t)
	page := get(console, "/").Body.String()
	bar, _, ok := strings.Cut(page, `class="tile"`)
	if !ok {
		t.Fatalf("page has no tile: %q", page)
	}
	_, bar, ok = strings.Cut(bar, "<header>")
	if !ok {
		t.Fatalf("the bar is not above the tiles: %q", page)
	}
	var got []string
	for _, m := range barControl.FindAllStringSubmatch(bar, -1) {
		got = append(got, m[1]+m[2]+m[3])
	}
	// The menu holds Connect and Disconnect.
	want := []string{"Menu", "connect", "disconnect", "dot", "Start all", "fault", "Stop all", "Reload"}
	if !slices.Equal(got, want) {
		t.Errorf("bar %v, want %v", got, want)
	}
	if !regexp.MustCompile(`<button [^>]*popovertarget="menu"[^>]*aria-label="Menu"`).MatchString(bar) ||
		!strings.Contains(bar, `<div id="menu" popover>`) {
		t.Errorf("the menu button does not open the menu: %q", bar)
	}
	m := script.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("page has no script: %q", page)
	}
	// The script's routes: the events it listens to and the commands it sends.
	for _, want := range []string{"new EventSource('/events')", "addEventListener('connection'", "'/connect'", "'/disconnect'", "'/start'", "'/stop'", "location.reload()"} {
		if !strings.Contains(m[1], want) {
			t.Errorf("script lacks %s: %q", want, m[1])
		}
	}
	if strings.Contains(m[1], "/status") {
		t.Errorf("script still polls /status: %q", m[1])
	}
	if strings.Contains(page, "<iframe") {
		t.Errorf("page has a frame: %q", page)
	}
}

// The page loads quiescent: shown disconnected, with no picture requested
// until its camera reports connected.
func TestPageRequestsNoPictureOnLoad(t *testing.T) {
	phone, _ := fed("pixel9")
	body, _ := relayed(t, "body", sonytest.NewCamera(t).Endpoint())
	console := New(t.Context(), []Named{phone, body}, saved(t, true))
	page := get(console, "/").Body.String()
	if !strings.Contains(page, `<body class="off">`) {
		t.Errorf("page does not load shown disconnected: %q", page)
	}
	if m := regexp.MustCompile(`<(img|video) [^>]*\bsrc=`).FindString(page); m != "" {
		t.Errorf("page requests a picture on load: %s", m)
	}
	if strings.Contains(script.FindStringSubmatch(page)[1], "querySelectorAll('video[data-whep]')) play(") {
		t.Errorf("script plays the streams on load: %q", page)
	}
}

// A tap shows what it asked for at once and sends its command whatever is
// outstanding: the script keeps each tile's intent, never disables a button
// or marks a tile busy, and orders nothing by a clock.
func TestScriptShowsATapsIntentAndNeverLocksOut(t *testing.T) {
	_, _, console := twoCameras(t)
	page := get(console, "/").Body.String()
	m := script.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("page has no script: %q", page)
	}
	if !strings.Contains(m[1], "intent") {
		t.Errorf("script keeps no intent: %q", m[1])
	}
	for _, gone := range []string{"disabled", "busy", "clock"} {
		if strings.Contains(m[1], gone) {
			t.Errorf("script still has %s: %q", gone, m[1])
		}
	}
	if strings.Contains(page, ".busy") {
		t.Errorf("page still styles a busy tile: %q", page)
	}
}

// retry is the one request made again: the first since its camera joined,
// 2 s after it fails.
var retry = regexp.MustCompile(`if \(first\) setTimeout\(\(\) => again\(t\), 2000\)`)

func TestScriptRequestsAFailedFirstPictureOnceMore2sLater(t *testing.T) {
	_, _, console := twoCameras(t)
	m := script.FindStringSubmatch(get(console, "/").Body.String())
	if m == nil {
		t.Fatal("page has no script")
	}
	if !retry.MatchString(m[1]) {
		t.Errorf("script does not request a failed first picture again 2 s later: %q", m[1])
	}
	if strings.Contains(m[1], "lost") {
		t.Errorf("script still requests a picture again on a camera's state: %q", m[1])
	}
}

var videoTag = regexp.MustCompile(`<video [^>]*>`)

func TestStreamedTileIsVideoFedByWHEP(t *testing.T) {
	phone, _ := fed("pixel9")
	body, _ := relayed(t, "body", sonytest.NewCamera(t).Endpoint())
	console := New(t.Context(), []Named{phone, body}, saved(t, false))
	page := get(console, "/").Body.String()
	if got, want := srcs(tileCamera, page), []string{"pixel9", "body"}; !slices.Equal(got, want) {
		t.Fatalf("tiles %v, want %v", got, want)
	}
	if got, want := srcs(imgSrc, page), []string{"/body/liveview"}; !slices.Equal(got, want) {
		t.Errorf("relayed pictures %v, want %v", got, want)
	}
	videos := videoTag.FindAllString(page, -1)
	if len(videos) != 1 {
		t.Fatalf("video tags %q, want one for the streamed camera", videos)
	}
	for _, attr := range []string{"autoplay", "muted", "playsinline", `data-whep=":8889/pixel9/whep"`} {
		if !strings.Contains(videos[0], attr) {
			t.Errorf("video %s lacks %s", videos[0], attr)
		}
	}
	// The streamed tile holds the video, the relayed one the image.
	tile, next := strings.Index(page, `data-camera="pixel9"`), strings.Index(page, `data-camera="body"`)
	if v := strings.Index(page, "<video"); v < tile || v > next {
		t.Errorf("the video is not in the streamed camera's tile: %q", page)
	}
	if !strings.Contains(script.FindStringSubmatch(page)[1], "location.hostname") {
		t.Errorf("script does not build the WHEP URL from the page's host: %q", page)
	}
	if rec := get(console, "/pixel9/liveview"); rec.Code != http.StatusNotFound {
		t.Errorf("streamed camera's liveview: status %d, want 404", rec.Code)
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

// stills is a liveview source that is not a Sony body: each session yields
// its frames once and then ends.
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
	f := newFeed()
	console := New(t.Context(), []Named{{Name: "phone", Picture: Relay(cam), Camera: f}}, saved(t, true))
	up(t, f)
	awaitUp(t, console, "phone")

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

func TestRelayedFailedStartHasNoSession(t *testing.T) {
	cam := &stills{startErr: errors.New("screen off")}
	lv, err := Relay(cam).Source.Liveview(t.Context())
	if err == nil || lv != nil {
		t.Errorf("Liveview = %#v, %v; want no session and the error", lv, err)
	}
	f := newFeed()
	console := New(t.Context(), []Named{{Name: "phone", Picture: Relay(cam), Camera: f}}, saved(t, true))
	up(t, f)
	awaitUp(t, console, "phone")
	if rec := get(console, "/phone/liveview"); rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "screen off") {
		t.Errorf("picture: status %d %q, want %d with the error", rec.Code, rec.Body, http.StatusBadGateway)
	}
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

// summary renders states one camera per word: its name, then "=recording"
// or "=idle" when connected (or "=?" when its state is unread),
// "=connecting", or "=off" when disconnected, then "!" when it carries an
// error.
func summary(states []state) string {
	var words []string
	for _, s := range states {
		word := s.Name + "=" + map[connState]string{connected: "?", connecting: "connecting", disconnected: "off"}[s.Connection]
		if s.Connection == connected && s.Recording != nil {
			word = s.Name + map[bool]string{true: "=recording", false: "=idle"}[*s.Recording]
		}
		if s.Error != "" {
			word += "!"
		}
		words = append(words, word)
	}
	return strings.Join(words, " ")
}

// states is the console's last known states, as a page gets them on
// connecting.
func states(c *console) []state {
	var states []state
	for _, k := range c.snapshot() {
		states = append(states, k.state)
	}
	return states
}

// events opens the console's event stream at base and returns its events as
// they arrive: an unnamed event's data, or a named one's name, a space and
// its data. The stream ends with the test.
func events(t *testing.T, base string) <-chan string {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); resp.Body.Close() })
	if ct := resp.Header.Get("Content-Type"); resp.StatusCode != http.StatusOK || !strings.HasPrefix(ct, "text/event-stream") || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("events: %s, Content-Type %q, Cache-Control %q; want 200 text/event-stream no-store", resp.Status, ct, resp.Header.Get("Cache-Control"))
	}
	lines := make(chan string, 16)
	go func() {
		defer close(lines)
		readEvents(bufio.NewScanner(resp.Body), lines)
	}()
	return lines
}

// readEvents sends each event scanner reads to events: an unnamed event's
// data, or a named one's name, a space and its data.
func readEvents(scanner *bufio.Scanner, events chan<- string) {
	name := ""
	for scanner.Scan() {
		line := scanner.Text()
		if n, ok := strings.CutPrefix(line, "event: "); ok {
			name = n + " "
		}
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			events <- name + data
			name = ""
		}
	}
}

// next is the next event's data, failing when none arrives in time.
func next(t *testing.T, lines <-chan string) string {
	t.Helper()
	select {
	case data, ok := <-lines:
		if !ok {
			t.Fatal("the event stream ended")
		}
		return data
	case <-time.After(5 * time.Second):
		t.Fatal("no event arrived")
		return ""
	}
}

func TestEventsSendTheConnectionAndEveryCamerasStateThenEachChange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		front, f := fed("front")
		side, s := fed("side")
		console := New(t.Context(), []Named{front, side}, saved(t, false))
		synctest.Wait()

		events := stream(t, console)
		synctest.Wait()
		for _, want := range []string{
			`connection {"connection":"disconnected","connecting":false}`,
			`{"name":"front","connection":"disconnected"}`,
			`{"name":"side","connection":"disconnected"}`,
		} {
			if got := waiting(t, events); got != want {
				t.Errorf("event %s, want %s", got, want)
			}
		}
		post(console, "/connect")
		synctest.Wait()
		assertLatest(t, events,
			`connection {"connection":"connected","connecting":true}`,
			`{"name":"front","connection":"connecting"}`,
			`{"name":"side","connection":"connecting"}`)
		fch, sch := f.watch(t, 0), s.watch(t, 0)
		fch <- true
		synctest.Wait()
		assertLatest(t, events, `{"name":"front","connection":"connected","recording":true}`)
		sch <- false
		synctest.Wait()
		assertLatest(t, events,
			`connection {"connection":"connected","connecting":false}`,
			`{"name":"side","connection":"connected","recording":false}`)

		// A second viewer gets the states as they are now.
		later := stream(t, console)
		synctest.Wait()
		for _, want := range []string{
			`connection {"connection":"connected","connecting":false}`,
			`{"name":"front","connection":"connected","recording":true}`,
			`{"name":"side","connection":"connected","recording":false}`,
		} {
			if got := waiting(t, later); got != want {
				t.Errorf("later viewer's event %s, want %s", got, want)
			}
		}
		// A repeat of the current state is not a change.
		sch <- false
		fch <- false
		synctest.Wait()
		assertLatest(t, events, `{"name":"front","connection":"connected","recording":false}`)
	})
}

// assertLatest drains the events waiting and checks that they end with each
// event of want for its subject (the connection or a camera), and carry no
// other subject's.
func assertLatest(t *testing.T, events <-chan string, want ...string) {
	t.Helper()
	subject := func(e string) string {
		if strings.HasPrefix(e, "connection ") {
			return "connection"
		}
		var s state
		json.Unmarshal([]byte(e), &s)
		return s.Name
	}
	latest := map[string]string{}
	for len(events) > 0 {
		e := <-events
		latest[subject(e)] = e
	}
	for _, w := range want {
		if got := latest[subject(w)]; got != w {
			t.Errorf("latest %s event %s, want %s", subject(w), got, w)
		}
		delete(latest, subject(w))
	}
	for _, e := range latest {
		t.Errorf("event %s, want none for its subject", e)
	}
}

// pipeWriter is a response whose body the test reads as it is written.
type pipeWriter struct {
	header http.Header
	body   *io.PipeWriter
}

func (p *pipeWriter) Header() http.Header         { return p.header }
func (p *pipeWriter) Write(b []byte) (int, error) { return p.body.Write(b) }
func (p *pipeWriter) WriteHeader(int)             {}
func (p *pipeWriter) Flush()                      {}

func TestEventsKeepTheStreamAliveEvery15s(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cam, f := fed("cam")
		console := New(t.Context(), []Named{cam}, saved(t, true))
		ch := f.watch(t, time.Second)
		ch <- false
		synctest.Wait()

		pr, pw := io.Pipe()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var wg sync.WaitGroup
		wg.Go(func() {
			console.ServeHTTP(&pipeWriter{header: http.Header{}, body: pw}, httptest.NewRequestWithContext(ctx, http.MethodGet, "/events", nil))
			pw.Close()
		})
		lines := bufio.NewScanner(pr)
		line := func() string {
			t.Helper()
			if !lines.Scan() {
				t.Fatal("the stream ended")
			}
			return lines.Text()
		}
		for _, want := range []string{"event: connection", `data: {"connection":"connected","connecting":false}`, ""} {
			if got := line(); got != want {
				t.Fatalf("line %q, want %q", got, want)
			}
		}
		if got, want := line(), `data: {"name":"cam","connection":"connected","recording":false}`; got != want {
			t.Fatalf("camera's line %q, want %q", got, want)
		}
		if got := line(); got != "" {
			t.Fatalf("line after the event %q, want the empty line ending it", got)
		}
		time.Sleep(15 * time.Second)
		if got := line(); !strings.HasPrefix(got, ":") {
			t.Errorf("line after 15 s %q, want a comment", got)
		}
		cancel()
		wg.Wait()
	})
}

func TestStartAllStartsTheCamerasNotRecording(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		front, f := fed("front")
		side, s := fed("side")
		gone, g := fed("gone")
		g.failWatch(errors.New("getEvent: connection refused"))
		console := New(t.Context(), []Named{front, side, gone}, saved(t, true))
		fch, sch := f.watch(t, time.Second), s.watch(t, time.Second)
		fch <- true
		sch <- false
		synctest.Wait()

		states := ask(t, console, http.MethodPost, "/start")
		if got, want := summary(states), "front=recording side=idle gone=off!"; got != want {
			t.Errorf("report %q, want %q", got, want)
		}
		if states[2].Error != "not connected" {
			t.Errorf("camera not connected: error %q, want not connected", states[2].Error)
		}
		if f.starts.Load() != 0 || s.starts.Load() != 1 || g.starts.Load() != 0 {
			t.Errorf("starts: front %d, side %d, gone %d; want only the connected camera not recording started",
				f.starts.Load(), s.starts.Load(), g.starts.Load())
		}
		// The side camera's change is reported once it arrives.
		sch <- true
		synctest.Wait()
		if got, want := summary(ask(t, console, http.MethodPost, "/start")), "front=recording side=recording gone=off!"; got != want {
			t.Errorf("report %q, want %q", got, want)
		}
		if s.starts.Load() != 1 {
			t.Errorf("side camera started %d times, want once", s.starts.Load())
		}
	})
}

func TestStopAllStopsTheCamerasNotIdle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		front, f := fed("front")
		side, s := fed("side")
		console := New(t.Context(), []Named{front, side}, saved(t, true))
		fch, sch := f.watch(t, time.Second), s.watch(t, time.Second)
		fch <- true
		sch <- false
		synctest.Wait()

		if got, want := summary(ask(t, console, http.MethodPost, "/stop")), "front=recording side=idle"; got != want {
			t.Errorf("report %q, want %q", got, want)
		}
		if f.stops.Load() != 1 || s.stops.Load() != 0 {
			t.Errorf("stops: front %d, side %d; want only the recording camera stopped", f.stops.Load(), s.stops.Load())
		}
	})
}

func TestOneCamerasRefusalIsItsOwnError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		front, f := fed("front")
		side, s := fed("side")
		s.startErr = errors.New("startMovieRec: camera error 40401 (Camera Not Ready)")
		console := New(t.Context(), []Named{front, side}, saved(t, true))
		fch, sch := f.watch(t, time.Second), s.watch(t, time.Second)
		fch <- false
		sch <- false
		synctest.Wait()
		var logged bytes.Buffer
		defer log.SetOutput(log.Writer())
		log.SetOutput(&logged)

		states := ask(t, console, http.MethodPost, "/start")
		if got, want := summary(states), "front=idle side=idle!"; got != want {
			t.Fatalf("report %q, want %q", got, want)
		}
		if states[1].Error != s.startErr.Error() {
			t.Errorf("refusing camera's error %q, want %q", states[1].Error, s.startErr)
		}
		if f.starts.Load() != 1 {
			t.Errorf("the other camera was started %d times, want once", f.starts.Load())
		}
		if !strings.Contains(logged.String(), "camera=side") || !strings.Contains(logged.String(), "40401") {
			t.Errorf("log lacks the refusal: %q", logged.String())
		}
	})
}

func TestOneCamerasCommand(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		front, f := fed("front")
		side, s := fed("side")
		console := New(t.Context(), []Named{front, side}, saved(t, true))
		fch, sch := f.watch(t, time.Second), s.watch(t, time.Second)
		fch <- false
		sch <- false
		synctest.Wait()

		if got, want := summary(ask(t, console, http.MethodPost, "/side/start")), "side=idle"; got != want {
			t.Errorf("start: report %q, want %q", got, want)
		}
		sch <- true
		synctest.Wait()
		if got, want := summary(ask(t, console, http.MethodPost, "/side/start")), "side=recording"; got != want {
			t.Errorf("start again: report %q, want %q", got, want)
		}
		if got, want := summary(ask(t, console, http.MethodPost, "/side/stop")), "side=recording"; got != want {
			t.Errorf("stop: report %q, want %q", got, want)
		}
		if s.starts.Load() != 1 || s.stops.Load() != 1 {
			t.Errorf("side camera: %d starts, %d stops; want one each", s.starts.Load(), s.stops.Load())
		}
		if f.starts.Load() != 0 || f.stops.Load() != 0 {
			t.Errorf("the other camera was commanded: %d starts, %d stops", f.starts.Load(), f.stops.Load())
		}
		for _, target := range []string{"/back/start", "/back/stop"} {
			rec := httptest.NewRecorder()
			console.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, target, nil))
			if rec.Code != http.StatusNotFound {
				t.Errorf("POST %s: status %d, want 404", target, rec.Code)
			}
		}
	})
}

func TestRefusedStopKeepsTheCamerasState(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cam, f := fed("cam")
		f.stopErr = errors.New("stopMovieRec: camera error 40401 (Camera Not Ready): Not <b>Ready</b>")
		console := New(t.Context(), []Named{cam}, saved(t, true))
		ch := f.watch(t, time.Second)
		ch <- true
		synctest.Wait()

		states := ask(t, console, http.MethodPost, "/cam/stop")
		if got, want := summary(states), "cam=recording!"; got != want {
			t.Fatalf("stop: report %q, want %q", got, want)
		}
		if states[0].Error != f.stopErr.Error() {
			t.Errorf("refusing camera's error %q, want %q", states[0].Error, f.stopErr)
		}
	})
}

// timed is a camera whose every command takes its own time: the time a real
// camera takes to answer, or to time out when err is set. Its state is idle
// and never changes.
type timed struct {
	takes     time.Duration
	err       error
	startedAt time.Time // when StartRecording was called
}

func (c *timed) wait() error {
	time.Sleep(c.takes)
	return c.err
}

func (c *timed) Watch(ctx context.Context) (<-chan bool, error) {
	ch := make(chan bool, 1)
	ch <- false
	context.AfterFunc(ctx, func() { close(ch) })
	return ch, nil
}

func (c *timed) StopRecording(context.Context) error { return c.wait() }
func (c *timed) StartRecording(context.Context) error {
	c.startedAt = time.Now()
	return c.wait()
}

// Each camera is commanded at the same moment, so the cameras' commands
// overlap and a report takes as long as its slowest camera, not the sum of
// them.
func TestCamerasAreCommandedTogether(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		asleep := &timed{takes: 15 * time.Second, err: errors.New("timed out")}
		slow := &timed{takes: 2 * time.Second}
		quick := &timed{}
		console := New(t.Context(), []Named{
			{Name: "asleep", Picture: Streamed{Path: "asleep"}, Camera: asleep},
			{Name: "slow", Picture: Streamed{Path: "slow"}, Camera: slow},
			{Name: "quick", Picture: Streamed{Path: "quick"}, Camera: quick},
		}, saved(t, true))
		synctest.Wait()

		began := time.Now()
		if got, want := summary(ask(t, console, http.MethodPost, "/start")), "asleep=idle! slow=idle quick=idle"; got != want {
			t.Errorf("start report %q, want %q", got, want)
		}
		if took := time.Since(began); took != 15*time.Second {
			t.Errorf("start took %v, want the slowest camera's 15s", took)
		}
		for name, cam := range map[string]*timed{"asleep": asleep, "slow": slow, "quick": quick} {
			if at := cam.startedAt.Sub(began); at != 0 {
				t.Errorf("the %s camera was started %v into the command, want at once", name, at)
			}
		}
	})
}
