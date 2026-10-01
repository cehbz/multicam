package sony

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/cehbz/multicam/internal/sony/sonytest"
)

func TestDefaultEndpoint(t *testing.T) {
	if want := "http://192.168.122.1:10000/sony/camera"; DefaultEndpoint != want {
		t.Errorf("DefaultEndpoint = %q, want %q", DefaultEndpoint, want)
	}
}

func TestLiveviewYieldsJPEGFramesInOrder(t *testing.T) {
	fake := sonytest.NewCamera(t)
	lv, err := NewCamera(fake.Endpoint()).Liveview(t.Context())
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
	lv, err := NewCamera(fake.Endpoint()).Liveview(t.Context())
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
	lv, err := NewCamera(fake.Endpoint()).Liveview(ctx)
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
	lv, err := NewCamera(fake.Endpoint()).Liveview(t.Context())
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

	lv, err := NewCamera(srv.URL).Liveview(t.Context())
	if err == nil || lv != nil {
		t.Errorf("Liveview = %v, %v; want an error and no session", lv, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"startLiveview", "stopLiveview"}; !slices.Equal(calls, want) {
		t.Errorf("camera calls %v, want %v", calls, want)
	}
}
