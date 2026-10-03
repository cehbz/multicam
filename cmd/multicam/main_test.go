package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cehbz/multicam/internal/rig"
	"github.com/cehbz/multicam/internal/sony/sonytest"
)

func TestConfigPath(t *testing.T) {
	tests := []struct {
		args    []string
		want    string
		wantErr bool
	}{
		{nil, "multicam.toml", false},
		{[]string{"/data/local/tmp/rig.toml"}, "/data/local/tmp/rig.toml", false},
		{[]string{"a.toml", "b.toml"}, "", true},
	}
	for _, tt := range tests {
		got, err := configPath(tt.args)
		if got != tt.want || (err != nil) != tt.wantErr {
			t.Errorf("configPath(%q) = %q, %v; want %q, error %v", tt.args, got, err, tt.want, tt.wantErr)
		}
	}
}

// firstParts reads the first n parts of the picture at url, then leaves it.
func firstParts(t *testing.T, url string, n int) [][]byte {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, params, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	parts := multipart.NewReader(resp.Body, params["boundary"])
	var frames [][]byte
	for i := range n {
		p, err := parts.NextPart()
		if err != nil {
			t.Fatalf("%s: %s, part %d: %v", url, resp.Status, i, err)
		}
		frame, _ := io.ReadAll(p)
		frames = append(frames, frame)
	}
	return frames
}

func TestRunServesSonyCamerasUntilStopped(t *testing.T) {
	sonyFake, second := sonytest.NewCamera(t), sonytest.NewCamera(t)
	config := filepath.Join(t.TempDir(), "multicam.toml")
	text := fmt.Sprintf("[[camera]]\nname = \"one\"\nkind = \"sony\"\nendpoint = %q\n[[camera]]\nname = \"two\"\nkind = \"sony\"\nendpoint = %q\n",
		sonyFake.Endpoint(), second.Endpoint())
	if err := os.WriteFile(config, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := rig.Load(config)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, ln, r) }()
	base := "http://" + ln.Addr().String()

	body := func(resp *http.Response, err error) string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s %s: %s %q", resp.Request.Method, resp.Request.URL.Path, resp.Status, b)
		}
		return string(b)
	}
	page := body(http.Get(base + "/"))
	last := -1
	for _, want := range []string{`data-camera="one"`, `<img src="/one/liveview"`, `data-camera="two"`, `<img src="/two/liveview"`} {
		i := strings.Index(page, want)
		if i <= last {
			t.Errorf("page lacks %s after byte %d: %q", want, last, page)
		}
		last = max(last, i)
	}
	// The console's events carry each camera's state, which it watches on
	// the body's long poll, and then each change. A camera's state is unread
	// until its first poll answers; the bodies answer in their own order.
	events := events(t, base)
	awaited := map[string]bool{`{"name":"one","recording":false}`: true, `{"name":"two","recording":false}`: true}
	for len(awaited) > 0 {
		got := nextEvent(t, events)
		if awaited[got] {
			delete(awaited, got)
		} else if !strings.Contains(got, `"status unread"`) {
			t.Fatalf("event %q, want a camera's unread or idle state", got)
		}
	}

	// Start on the second camera alone: the report carries the state the
	// console knew; the change arrives as an event once the body reports it.
	if got, want := body(http.Post(base+"/two/start", "", nil)), `{"cameras":[{"name":"two","recording":false}]}`+"\n"; got != want {
		t.Errorf("report of the second camera's start %q, want %q", got, want)
	}
	second.Push("MovieRecording")
	if got, want := nextEvent(t, events), `{"name":"two","recording":true}`; got != want {
		t.Errorf("event %q, want %q", got, want)
	}
	if got, want := sonyFake.Calls(), []string{"getEvent@1.3", "getEvent@1.3+"}; !slices.Equal(got, want) {
		t.Errorf("first camera's calls after the second's Start %v, want its watch alone, %v", got, want)
	}
	if !slices.Contains(second.Calls(), "startMovieRec@1.0") {
		t.Errorf("second camera's calls %v lack its start", second.Calls())
	}

	// Each picture relays its own camera.
	resp, err := http.Get(base + "/one/liveview")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, params, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	parts := multipart.NewReader(resp.Body, params["boundary"])
	for i, want := range sonyFake.Frames {
		p, err := parts.NextPart()
		if err != nil {
			t.Fatalf("stream %s, part %d: %v", resp.Status, i, err)
		}
		if got, _ := io.ReadAll(p); !bytes.Equal(got, want) {
			t.Fatalf("part %d: %d bytes, want the camera's frame %d (%d bytes)", i, len(got), i, len(want))
		}
	}

	// Stopping with a viewer attached ends its liveview session before run
	// returns.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run still serving 5 s after its context ended")
	}
	if got, want := sonyFake.Calls(), []string{"getEvent@1.3", "getEvent@1.3+", "startLiveview@1.0", "stopLiveview@1.0"}; !slices.Equal(got, want) {
		t.Errorf("first camera's calls %v, want %v", got, want)
	}
}

// events opens the console's event stream at base and returns its events'
// data as it arrives, until the test ends.
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
	if ct := resp.Header.Get("Content-Type"); resp.StatusCode != http.StatusOK || !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("events: %s, Content-Type %q; want 200 text/event-stream", resp.Status, ct)
	}
	lines := make(chan string, 16)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			if data, ok := strings.CutPrefix(scanner.Text(), "data: "); ok {
				lines <- data
			}
		}
	}()
	return lines
}

// nextEvent is the next event's data, failing when none arrives in time.
func nextEvent(t *testing.T, lines <-chan string) string {
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
