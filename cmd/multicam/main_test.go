package main

import (
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

// The adb commands a Pixel camera sends, after "-s <address>".
const (
	pixelStatus  = "shell dumpsys audio | grep 'source client=CAMCORDER' || true"
	pixelFront   = "shell dumpsys activity activities | grep 'topResumedActivity=.*com.google.android.GoogleCamera/' || true"
	pixelShutter = "shell input keyevent 24"
)

var pixelScreen = append([]byte{0xff, 0xd8, 0xff, 0xe0}, "pixel screen"...)

// fakePixel writes an adb stand-in for a Pixel at address with Pixel Camera
// in front: its screenshot is pixelScreen and its shutter key toggles the
// recording its status reports. commands lists what the phone was sent.
func fakePixel(t *testing.T, address string) (adb string, commands func() []string) {
	dir := t.TempDir()
	adb = filepath.Join(dir, "adb")
	script := `#!/bin/sh
echo "$*" >> '` + dir + `/log'
case "$*" in
"connect ` + address + `") echo "already connected to ` + address + `" ;;
"-s ` + address + ` exec-out screencap -j") printf '\377\330\377\340pixel screen' ;;
"-s ` + address + ` ` + pixelStatus + `")
	if [ -e '` + dir + `/recording' ]; then
		echo "  session:20857 -- source client=CAMCORDER, dev=2ch 48000Hz ENCODING_PCM_16BIT -- uid:10171 -- patch:2329 -- pack:com.google.android.GoogleCamera -- format client=2ch 48000Hz ENCODING_PCM_16BIT, dev=2ch 48000Hz ENCODING_PCM_16BIT"
	fi ;;
"-s ` + address + ` ` + pixelFront + `")
	echo "topResumedActivity=ActivityRecord{142494291 u0 com.google.android.GoogleCamera/com.google.android.apps.camera.activity.main.CameraActivity t53927}" ;;
"-s ` + address + ` ` + pixelShutter + `")
	if [ -e '` + dir + `/recording' ]; then rm '` + dir + `/recording'; else touch '` + dir + `/recording'; fi ;;
*) echo "fake adb: unexpected command: $*" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(adb, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return adb, func() []string {
		log, _ := os.ReadFile(filepath.Join(dir, "log"))
		var sent []string
		for line := range strings.Lines(string(log)) {
			if command, ok := strings.CutPrefix(strings.TrimSuffix(line, "\n"), "-s "+address+" "); ok {
				sent = append(sent, command)
			}
		}
		return sent
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

func TestRunServesSonyAndPixelCamerasUntilStopped(t *testing.T) {
	sonyFake := sonytest.NewCamera(t)
	const address = "192.168.1.109:41419"
	adb, pixelCommands := fakePixel(t, address)
	config := filepath.Join(t.TempDir(), "multicam.toml")
	text := fmt.Sprintf("[adb]\npath = %q\nkey_dir = %q\n[[camera]]\nname = \"one\"\nkind = \"sony\"\nendpoint = %q\n[[camera]]\nname = \"pixel9\"\nkind = \"pixel\"\naddress = %q\n",
		adb, t.TempDir(), sonyFake.Endpoint(), address)
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
	for _, want := range []string{`<img src="/one/liveview"`, `<iframe src="/one/record"`, `<img src="/pixel9/liveview"`, `<iframe src="/pixel9/record"`} {
		i := strings.Index(page, want)
		if i <= last {
			t.Errorf("page lacks %s after byte %d: %q", want, last, page)
		}
		last = max(last, i)
	}
	for _, path := range []string{"/one/record", "/pixel9/record"} {
		if control := body(http.Get(base + path)); !strings.Contains(control, "<button>Start</button>") {
			t.Fatalf("%s: %q, want a Start button", path, control)
		}
	}

	// Start on the Pixel: the browser is redirected to its control.
	if control := body(http.Post(base+"/pixel9/record/start", "", nil)); !strings.Contains(control, "recording <button>Stop</button>") {
		t.Errorf("Pixel's control after Start: %q, want recording with a Stop button", control)
	}
	wantPixel := []string{
		pixelStatus,                                        // its control drawn
		pixelStatus, pixelFront, pixelShutter, pixelStatus, // Start
		pixelStatus, // its control redrawn
	}
	if got := pixelCommands(); !slices.Equal(got, wantPixel) {
		t.Errorf("adb commands to the Pixel:\n got %q\nwant %q", got, wantPixel)
	}
	if got, want := sonyFake.Calls(), []string{"getEvent@1.3"}; !slices.Equal(got, want) {
		t.Errorf("Sony camera's calls after the Pixel's Start %v, want %v", got, want)
	}

	// Each picture relays its own camera.
	for i, frame := range firstParts(t, base+"/pixel9/liveview", 2) {
		if !bytes.Equal(frame, pixelScreen) {
			t.Errorf("Pixel picture part %d = %q, want its screenshot", i, frame)
		}
	}
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
	if got, want := sonyFake.Calls(), []string{"getEvent@1.3", "startLiveview@1.0", "stopLiveview@1.0"}; !slices.Equal(got, want) {
		t.Errorf("Sony camera's calls %v, want %v", got, want)
	}
}
