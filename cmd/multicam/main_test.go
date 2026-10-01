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

	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(page), `<img src="/liveview"`) {
		t.Fatalf("page: %s %q, want 200 with the liveview image", resp.Status, page)
	}

	resp, err = http.Get(base + "/liveview")
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
	if got, want := fake.Calls(), []string{"startLiveview@1.0", "stopLiveview@1.0"}; !slices.Equal(got, want) {
		t.Errorf("camera calls %v, want %v", got, want)
	}
}
