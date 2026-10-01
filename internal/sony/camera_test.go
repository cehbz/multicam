package sony

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cehbz/multicam/internal/sony/sonytest"
)

// newCamera returns the camera at fake, not bound to an interface.
func newCamera(t *testing.T, fake *sonytest.Camera) *Camera {
	t.Helper()
	cam, err := NewCamera(fake.Endpoint(), "")
	if err != nil {
		t.Fatal(err)
	}
	return cam
}

func TestDefaultEndpoint(t *testing.T) {
	if want := "http://192.168.122.1:10000/sony/camera"; DefaultEndpoint != want {
		t.Errorf("DefaultEndpoint = %q, want %q", DefaultEndpoint, want)
	}
}

func TestLiveviewYieldsJPEGFramesInOrder(t *testing.T) {
	fake := sonytest.NewCamera(t)
	lv, err := newCamera(t, fake).Liveview(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer lv.Close()
	// Twice round the fake's frames and one more; the fake's frame-info
	// packets must not show up between them.
	for i := range 2*len(fake.Frames) + 1 {
		got, err := lv.Next()
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if want := fake.Frames[i%len(fake.Frames)]; !bytes.Equal(got, want) {
			t.Fatalf("frame %d: %d bytes starting % x, want the fake's frame %d (%d bytes)",
				i, len(got), got[:min(len(got), 8)], i%len(fake.Frames), len(want))
		}
	}
}

func TestLiveviewCloseStopsLiveview(t *testing.T) {
	fake := sonytest.NewCamera(t)
	lv, err := newCamera(t, fake).Liveview(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lv.Next(); err != nil {
		t.Fatalf("first frame: %v", err)
	}
	if err := lv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if f, err := lv.Next(); err == nil {
		t.Errorf("Next after Close returned a %d-byte frame, want an error", len(f))
	}
	if got, want := fake.Calls(), []string{"startLiveview@1.0", "stopLiveview@1.0"}; !slices.Equal(got, want) {
		t.Errorf("camera calls %v, want %v", got, want)
	}
}

func TestLiveviewCancelEndsStream(t *testing.T) {
	fake := sonytest.NewCamera(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	lv, err := newCamera(t, fake).Liveview(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Several frames arrive before the first read, so it leaves later ones
	// buffered; they are not yielded after the cancel.
	time.Sleep(150 * time.Millisecond)
	if _, err := lv.Next(); err != nil {
		t.Fatalf("first frame: %v", err)
	}
	cancel()
	if f, err := lv.Next(); !errors.Is(err, context.Canceled) {
		t.Errorf("Next after cancel: %d-byte frame, err = %v, want context.Canceled", len(f), err)
	}
	if err := lv.Close(); err != nil {
		t.Errorf("Close after cancel: %v", err)
	}
	if got, want := fake.Calls(), []string{"startLiveview@1.0", "stopLiveview@1.0"}; !slices.Equal(got, want) {
		t.Errorf("camera calls %v, want %v", got, want)
	}
}

func TestLiveviewStartError(t *testing.T) {
	fake := sonytest.NewCamera(t)
	fake.Fail("startLiveview", 40401, "Camera Not Ready")
	lv, err := newCamera(t, fake).Liveview(t.Context())
	var camErr *Error
	if !errors.As(err, &camErr) || camErr.Code != 40401 {
		t.Errorf("err = %v, want camera error 40401", err)
	}
	if lv != nil {
		t.Errorf("Liveview returned a session alongside err %v", err)
	}
	if got, want := fake.Calls(), []string{"startLiveview@1.0"}; !slices.Equal(got, want) {
		t.Errorf("camera calls %v, want %v", got, want)
	}
}

// A camera whose stream URL can't be opened is left with liveview stopped.
func TestLiveviewStreamOpenFailureStopsLiveview(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var req struct{ Method string }
		json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		calls = append(calls, req.Method)
		mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"result": []string{srv.URL + "/liveviewstream"}, "id": 1})
	}))
	defer srv.Close()

	cam, err := NewCamera(srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	lv, err := cam.Liveview(t.Context())
	if err == nil || lv != nil {
		t.Errorf("Liveview = %v, %v; want an error and no session", lv, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"startLiveview", "stopLiveview"}; !slices.Equal(calls, want) {
		t.Errorf("camera calls %v, want %v", calls, want)
	}
}

func TestRecordingFollowsStartAndStop(t *testing.T) {
	fake := sonytest.NewCamera(t)
	cam := newCamera(t, fake)
	recording := func(when string, want bool) {
		t.Helper()
		got, err := cam.Recording(t.Context())
		if err != nil {
			t.Fatalf("%s: Recording: %v", when, err)
		}
		if got != want {
			t.Errorf("%s: Recording = %v, want %v", when, got, want)
		}
	}
	recording("before start", false)
	if err := cam.StartRecording(t.Context()); err != nil {
		t.Fatalf("StartRecording: %v", err)
	}
	recording("after start", true)
	if err := cam.StopRecording(t.Context()); err != nil {
		t.Fatalf("StopRecording: %v", err)
	}
	recording("after stop", false)
	want := []string{"getEvent@1.3", "startMovieRec@1.0", "getEvent@1.3", "stopMovieRec@1.0", "getEvent@1.3"}
	if got := fake.Calls(); !slices.Equal(got, want) {
		t.Errorf("camera calls %v, want %v", got, want)
	}
}

func TestRecordingRefusalIsTheCameraError(t *testing.T) {
	tests := []struct {
		method string
		call   func(*Camera, context.Context) error
	}{
		{"startMovieRec", (*Camera).StartRecording},
		{"stopMovieRec", (*Camera).StopRecording},
		{"getEvent", func(c *Camera, ctx context.Context) error { _, err := c.Recording(ctx); return err }},
	}
	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			fake := sonytest.NewCamera(t)
			fake.Fail(tt.method, 40401, "Camera Not Ready")
			err := tt.call(newCamera(t, fake), t.Context())
			var camErr *Error
			if !errors.As(err, &camErr) || camErr.Code != 40401 {
				t.Errorf("err = %v, want camera error 40401", err)
			}
		})
	}
}

// Statuses per Sony's sample client; the RX10M4 and RX100M6 captures show only
// IDLE and MovieRecording around startMovieRec and stopMovieRec.
func TestRecordingByCameraStatus(t *testing.T) {
	tests := []struct {
		status string
		want   bool
	}{
		{"IDLE", false},
		{"MovieWaitRecStart", true},
		{"MovieRecording", true},
		{"MovieWaitRecStop", false},
		{"MovieSaving", false},
		{"NotReady", false},
		{"Error", false},
		{"StillCapturing", false},
	}
	fake := sonytest.NewCamera(t)
	cam := newCamera(t, fake)
	for _, tt := range tests {
		fake.SetCameraStatus(tt.status)
		got, err := cam.Recording(t.Context())
		if err != nil {
			t.Fatalf("%s: %v", tt.status, err)
		}
		if got != tt.want {
			t.Errorf("cameraStatus %s: Recording = %v, want %v", tt.status, got, tt.want)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Both Sony bodies answer at the same address, so a camera's calls and its
// liveview stream stay off the transport other HTTP clients share.
func TestCameraConnectionsAreItsOwn(t *testing.T) {
	fake := sonytest.NewCamera(t)
	cam := newCamera(t, fake)
	defer func(rt http.RoundTripper) { http.DefaultTransport = rt }(http.DefaultTransport)
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("%s %s went through the shared default transport", r.Method, r.URL.Path)
	})

	lv, err := cam.Liveview(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lv.Next(); err != nil {
		t.Errorf("first frame: %v", err)
	}
	if err := lv.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// recordBinds replaces the system's bind with one that records the interface
// of each connection it is asked to bind and answers with err.
func recordBinds(t *testing.T, err error) func() []string {
	var mu sync.Mutex
	var ifaces []string
	system := bindToDevice
	t.Cleanup(func() { bindToDevice = system })
	bindToDevice = func(fd int, iface string) error {
		mu.Lock()
		defer mu.Unlock()
		ifaces = append(ifaces, iface)
		return err
	}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(ifaces)
	}
}

func TestCameraBindsItsConnectionsToItsInterface(t *testing.T) {
	// One address for both cameras, as on the rig.
	fake := sonytest.NewCamera(t)
	binds := recordBinds(t, nil)
	first, err := NewCamera(fake.Endpoint(), "wlan1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewCamera(fake.Endpoint(), "cam2")
	if err != nil {
		t.Fatal(err)
	}

	lv, err := first.Liveview(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lv.Next(); err != nil {
		t.Errorf("first frame: %v", err)
	}
	if err := lv.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if got := binds(); len(got) == 0 || slices.ContainsFunc(got, func(iface string) bool { return iface != "wlan1" }) {
		t.Errorf("first camera's connections bound to %v, want each to wlan1", got)
	}

	// The second camera does not reuse the first's idle connection.
	if _, err := second.Recording(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := binds(); !slices.Contains(got, "cam2") {
		t.Errorf("connections bound to %v, want the second camera's bound to cam2", got)
	}
}

func TestCameraBindFailureNamesTheInterface(t *testing.T) {
	fake := sonytest.NewCamera(t)
	recordBinds(t, errors.New("no such device"))
	cam, err := NewCamera(fake.Endpoint(), "cam2")
	if err != nil {
		t.Fatal(err)
	}
	_, err = cam.Recording(t.Context())
	if err == nil || !strings.Contains(err.Error(), "cam2") || !strings.Contains(err.Error(), "no such device") {
		t.Errorf("err = %v, want the bind failure on cam2", err)
	}
	if calls := fake.Calls(); len(calls) != 0 {
		t.Errorf("camera reached over an unbound connection: %v", calls)
	}
}

func TestCameraOnAnInterfaceNeedsLinux(t *testing.T) {
	if runtime.GOOS == "linux" || runtime.GOOS == "android" {
		t.Skip("this system binds connections to interfaces")
	}
	cam, err := NewCamera(DefaultEndpoint, "wlan1")
	if err == nil || cam != nil || !strings.Contains(err.Error(), "wlan1") || !strings.Contains(err.Error(), runtime.GOOS) {
		t.Errorf("NewCamera = %v, %v; want an error naming wlan1 and %s", cam, err, runtime.GOOS)
	}
}
