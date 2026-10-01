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
	return fake, New(sony.NewCamera(fake.Endpoint()).Liveview)
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
