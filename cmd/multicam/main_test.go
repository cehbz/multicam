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
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cehbz/multicam/internal/console"
	"github.com/cehbz/multicam/internal/mediamtx"
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
	sonyFake.ListLiveview(true)
	second.ListLiveview(true)
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
	for _, want := range []string{`data-camera="one"`, `<img data-liveview="/one/liveview"`, `data-camera="two"`, `<img data-liveview="/two/liveview"`} {
		i := strings.Index(page, want)
		if i <= last {
			t.Errorf("page lacks %s after byte %d: %q", want, last, page)
		}
		last = max(last, i)
	}
	// With nothing saved the server starts Disconnected. Connect watches
	// each body on its long poll, and the console's events carry each
	// camera's state and then each change.
	events := events(t, base)
	awaitEvent(t, events, `{"connection":"disconnected","connecting":false}`)
	// Each body's picture is playable once its feed delivers frames.
	want := regexp.MustCompile(`^\{"connection":"connected","cameras":\[\{"name":"one","connection":"connected","recording":false(,"picture":true)?\},\{"name":"two","connection":"connected","recording":false(,"picture":true)?\}\]\}\n$`)
	if got := body(http.Post(base+"/connect", "", nil)); !want.MatchString(got) {
		t.Errorf("Connect's answer %q, want both cameras connected and idle", got)
	}
	awaitEvent(t, events, `{"name":"one","connection":"connected","recording":false,"picture":true}`, `{"name":"two","connection":"connected","recording":false,"picture":true}`)
	if connected, err := r.State.Connected(); !connected || err != nil {
		t.Errorf("saved state %v, %v; want Connected", connected, err)
	}

	// Start on the second camera alone: the report carries the state the
	// console knew; the change arrives as an event once the body reports it.
	if got, want := body(http.Post(base+"/two/start", "", nil)), `{"cameras":[{"name":"two","connection":"connected","recording":false,"picture":true}]}`+"\n"; got != want {
		t.Errorf("report of the second camera's start %q, want %q", got, want)
	}
	second.Push("MovieRecording")
	awaitEvent(t, events, `{"name":"two","connection":"connected","recording":true,"picture":true}`)
	if got, want := slices.Sorted(slices.Values(sonyFake.Calls())), []string{"getEvent@1.3", "getEvent@1.3+", "startLiveview@1.0"}; !slices.Equal(got, want) {
		t.Errorf("first camera's calls after the second's Start %v, want its watch and its picture's alone, %v", got, want)
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

	// Stopping ends each body's liveview, with a viewer attached or none,
	// before run returns.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run still serving 5 s after its context ended")
	}
	if got, want := slices.Sorted(slices.Values(sonyFake.Calls())), []string{"getEvent@1.3", "getEvent@1.3+", "startLiveview@1.0", "stopLiveview@1.0"}; !slices.Equal(got, want) {
		t.Errorf("first camera's calls %v, want %v", got, want)
	}
	if calls := second.Calls(); !slices.Contains(calls, "stopLiveview@1.0") {
		t.Errorf("second camera's calls %v lack its stopLiveview", calls)
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

// awaitEvent waits for an event with each data of want, in any order,
// skipping the others, failing when they don't arrive in time.
func awaitEvent(t *testing.T, lines <-chan string, want ...string) {
	t.Helper()
	awaited := map[string]bool{}
	for _, w := range want {
		awaited[w] = true
	}
	timeout := time.After(5 * time.Second)
	for len(awaited) > 0 {
		select {
		case data, ok := <-lines:
			if !ok {
				t.Fatal("the event stream ended")
			}
			delete(awaited, data)
		case <-timeout:
			t.Fatalf("events %v did not arrive", awaited)
		}
	}
}

func TestRunKeepsMediaMTXRunningAndEndsItWithTheServer(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	exe := filepath.Join(dir, "fakemtx")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\necho $$ > "+pidFile+"\nexec sleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &rig.Rig{
		MediaMTX: &mediamtx.Server{Path: exe, Dir: dir, Log: filepath.Join(dir, "log"), RestartDelay: time.Second},
		State:    console.StateFile(filepath.Join(dir, "connection.state")),
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- run(ctx, ln, r) }()
	var pid int
	for deadline := time.Now().Add(5 * time.Second); pid == 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("MediaMTX did not start")
		}
		b, _ := os.ReadFile(pidFile)
		if strings.HasSuffix(string(b), "\n") {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return")
	}
	if syscall.Kill(pid, 0) == nil {
		syscall.Kill(pid, syscall.SIGKILL)
		t.Errorf("MediaMTX %d still running after run returned", pid)
	}
}
