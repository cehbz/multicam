package main

import (
	"bytes"
	"context"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cehbz/multicam/internal/sony/sonytest"
)

func TestRunServesTheConsoleUntilStopped(t *testing.T) {
	fake := sonytest.NewCamera(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, ln, fake.Endpoint()) }()
	base := "http://" + ln.Addr().String()

	get := func(path string) (*http.Response, string) {
		t.Helper()
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp, string(body)
	}
	if resp, page := get("/"); resp.StatusCode != http.StatusOK || !strings.Contains(page, `<img src="/liveview"`) || !strings.Contains(page, `<iframe src="/record"`) {
		t.Fatalf("page: %s %q, want 200 with the liveview image and the record control", resp.Status, page)
	}
	if resp, control := get("/record"); resp.StatusCode != http.StatusOK || !strings.Contains(control, "<button>Start</button>") {
		t.Fatalf("record control: %s %q, want 200 with a Start button", resp.Status, control)
	}

	resp, err := http.Get(base + "/liveview")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, params, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	parts := multipart.NewReader(resp.Body, params["boundary"])
	for i, want := range fake.Frames {
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
	if got, want := fake.Calls(), []string{"getEvent@1.3", "startLiveview@1.0", "stopLiveview@1.0"}; !slices.Equal(got, want) {
		t.Errorf("camera calls %v, want %v", got, want)
	}
}
