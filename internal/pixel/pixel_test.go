package pixel

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cehbz/multicam/internal/console"
)

// The adb commands the camera sends, as the fake phone sees them.
const (
	screenshot = "exec-out screencap -j"
	status     = "shell dumpsys audio | grep 'source client=CAMCORDER' || true"
	frontApp   = "shell dumpsys activity activities | grep 'topResumedActivity=.*com.google.android.GoogleCamera/' || true"
	launch     = "shell am start -W -a android.media.action.VIDEO_CAMERA -p com.google.android.GoogleCamera"
	shutter    = "shell input keyevent 24"
)

// Lines measured on the Pixel 9 Pro XL (Android 17).
const (
	// dumpsys audio, RecordActivityMonitor, while recording.
	audioRecording = "riid 11319; active? true\n" +
		"  session:20857 -- source client=CAMCORDER, dev=2ch 48000Hz ENCODING_PCM_16BIT -- uid:10171 -- patch:2329 -- pack:com.google.android.GoogleCamera -- format client=2ch 48000Hz ENCODING_PCM_16BIT, dev=2ch 48000Hz ENCODING_PCM_16BIT\n"
	// Idle in video mode.
	audioIdle = "riid 11319; active? false\n"
	// After a stop: the events log.
	audioStopped = "10-01 16:19:36:452 rec stop riid:11319 uid:10171 session:20857 src:CAMCORDER not silenced pack:com.google.android.GoogleCamera\n"
	// dumpsys activity activities with Pixel Camera in front.
	activitiesCamera = "topResumedActivity=ActivityRecord{142494291 u0 com.google.android.GoogleCamera/com.google.android.apps.camera.activity.main.CameraActivity t53927}\n"
)

// phone is a fake Pixel behind adb. It records every command and answers
// the ones above from its state; the two dumpsys filters run on it, as they
// do on the real phone.
type phone struct {
	mu       sync.Mutex
	commands []string

	audio      string   // dumpsys audio
	activities string   // dumpsys activity activities
	locked     bool     // a launch leaves Pixel Camera behind the lock screen
	photoMode  bool     // the shutter doesn't start a recording
	shots      [][]byte // screencap outputs in order; adb fails once they run out
	hang       bool     // screencap runs until its context ends
	fail       error    // every command fails with it

	volumeUps int // shutter keys that landed outside Pixel Camera
}

var cameraInFront = regexp.MustCompile(`topResumedActivity=.*com.google.android.GoogleCamera/`)

// grep returns the lines of text that match.
func grep(text string, match func(string) bool) []byte {
	var out []string
	for line := range strings.Lines(text) {
		if match(line) {
			out = append(out, line)
		}
	}
	return []byte(strings.Join(out, ""))
}

func (p *phone) run(ctx context.Context, args ...string) ([]byte, error) {
	p.mu.Lock()
	command := strings.Join(args, " ")
	p.commands = append(p.commands, command)
	if p.fail != nil {
		p.mu.Unlock()
		return nil, p.fail
	}
	if command == screenshot && p.hang {
		p.mu.Unlock()
		<-ctx.Done()
		return nil, ctx.Err()
	}
	defer p.mu.Unlock()
	front := cameraInFront.MatchString(p.activities)
	switch command {
	case screenshot:
		if len(p.shots) == 0 {
			return nil, errors.New("adb: device offline")
		}
		shot := p.shots[0]
		p.shots = p.shots[1:]
		return shot, nil
	case status:
		return grep(p.audio, func(l string) bool { return strings.Contains(l, "source client=CAMCORDER") }), nil
	case frontApp:
		return grep(p.activities, cameraInFront.MatchString), nil
	case launch:
		if !p.locked {
			p.activities = activitiesCamera
		}
		return []byte("Status: ok\n"), nil
	case shutter:
		switch {
		case !front:
			p.volumeUps++
		case p.photoMode:
		case p.audio == audioRecording:
			p.audio = audioStopped
		default:
			p.audio = audioRecording
		}
		return nil, nil
	}
	return nil, fmt.Errorf("fake phone: unexpected adb command %q", command)
}

func (p *phone) sent() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.commands)
}

// wantCommands checks the commands sent and that no shutter key landed
// outside Pixel Camera.
func (p *phone) wantCommands(t *testing.T, want ...string) {
	t.Helper()
	if got := p.sent(); !slices.Equal(got, want) {
		t.Errorf("adb commands:\n got %q\nwant %q", got, want)
	}
	if p.volumeUps != 0 {
		t.Errorf("%d shutter keys sent without Pixel Camera in front", p.volumeUps)
	}
}

func TestRecordingStatus(t *testing.T) {
	tests := []struct {
		name  string
		audio string
		want  bool
	}{
		{"recording", audioRecording, true},
		{"idle in video mode", audioIdle, false},
		{"after a stop", audioStopped, false},
		{"nothing from the recorder monitor", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &phone{audio: tt.audio}
			got, err := NewCamera(p.run).Recording(t.Context())
			if err != nil || got != tt.want {
				t.Errorf("Recording = %v, %v; want %v", got, err, tt.want)
			}
			p.wantCommands(t, status)
		})
	}
}

func TestStartRecording(t *testing.T) {
	t.Run("camera in front", func(t *testing.T) {
		p := &phone{audio: audioIdle, activities: activitiesCamera}
		if err := NewCamera(p.run).StartRecording(t.Context()); err != nil {
			t.Fatal(err)
		}
		p.wantCommands(t, status, frontApp, shutter, status)
		if p.audio != audioRecording {
			t.Errorf("phone is not recording")
		}
	})
	t.Run("camera launched first", func(t *testing.T) {
		p := &phone{audio: audioIdle}
		if err := NewCamera(p.run).StartRecording(t.Context()); err != nil {
			t.Fatal(err)
		}
		p.wantCommands(t, status, frontApp, launch, frontApp, shutter, status)
	})
	t.Run("already recording", func(t *testing.T) {
		p := &phone{audio: audioRecording, activities: activitiesCamera}
		if err := NewCamera(p.run).StartRecording(t.Context()); err != nil {
			t.Fatal(err)
		}
		p.wantCommands(t, status)
	})
}

func TestStopRecording(t *testing.T) {
	t.Run("camera in front", func(t *testing.T) {
		p := &phone{audio: audioRecording, activities: activitiesCamera}
		if err := NewCamera(p.run).StopRecording(t.Context()); err != nil {
			t.Fatal(err)
		}
		p.wantCommands(t, status, frontApp, shutter, status)
		if p.audio != audioStopped {
			t.Errorf("phone is still recording")
		}
	})
	t.Run("not recording", func(t *testing.T) {
		for _, audio := range []string{audioIdle, audioStopped} {
			p := &phone{audio: audio, activities: activitiesCamera}
			if err := NewCamera(p.run).StopRecording(t.Context()); err != nil {
				t.Fatal(err)
			}
			p.wantCommands(t, status)
		}
	})
}

// The shutter is the volume-up key: outside Pixel Camera it changes the
// phone's volume, so it is never sent unless the camera is in front.
func TestNoShutterUnlessCameraInFront(t *testing.T) {
	for name, do := range map[string]func(*Camera, context.Context) error{
		"start": (*Camera).StartRecording,
		"stop":  (*Camera).StopRecording,
	} {
		t.Run(name, func(t *testing.T) {
			p := &phone{audio: audioIdle, locked: true}
			if name == "stop" {
				p.audio = audioRecording
			}
			err := do(NewCamera(p.run), t.Context())
			if err == nil || !strings.Contains(err.Error(), "unlock the phone and open Pixel Camera in Video mode") {
				t.Errorf("err = %v, want one telling the user to unlock the phone and open Pixel Camera in Video mode", err)
			}
			p.wantCommands(t, status, frontApp, launch, frontApp)
		})
	}
}

func TestRecordingThatDoesNotChangeIsAnError(t *testing.T) {
	for name, tt := range map[string]struct {
		audio string
		do    func(*Camera, context.Context) error
		want  string
	}{
		"start": {audioIdle, (*Camera).StartRecording, "recording did not start: check that Pixel Camera is in Video mode"},
		"stop":  {audioRecording, (*Camera).StopRecording, "recording did not stop"},
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				p := &phone{audio: tt.audio, activities: activitiesCamera, photoMode: true}
				began := time.Now()
				err := tt.do(NewCamera(p.run), t.Context())
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Errorf("err = %v, want %q", err, tt.want)
				}
				if waited := time.Since(began); waited < 2*time.Second || waited > 10*time.Second {
					t.Errorf("gave up after %v, want a few seconds", waited)
				}
				sent := p.sent()
				if len(sent) < 4 || !slices.Equal(sent[:3], []string{status, frontApp, shutter}) || slices.Contains(sent[3:], shutter) {
					t.Errorf("adb commands %q: want one shutter key after the checks, then status reads only", sent)
				}
				if p.volumeUps != 0 {
					t.Errorf("%d shutter keys sent without Pixel Camera in front", p.volumeUps)
				}
			})
		})
	}
}

func TestADBErrorsComeBack(t *testing.T) {
	offline := errors.New("adb: device '192.168.1.9:5555' not found")
	for name, do := range map[string]func(*Camera, context.Context) error{
		"recording": func(c *Camera, ctx context.Context) error { _, err := c.Recording(ctx); return err },
		"start":     (*Camera).StartRecording,
		"stop":      (*Camera).StopRecording,
		"liveview":  func(c *Camera, ctx context.Context) error { _, err := c.Liveview(ctx); return err },
	} {
		t.Run(name, func(t *testing.T) {
			p := &phone{fail: offline}
			if err := do(NewCamera(p.run), t.Context()); !errors.Is(err, offline) {
				t.Errorf("err = %v, want adb's %v", err, offline)
			}
			if sent := p.sent(); len(sent) != 1 {
				t.Errorf("adb commands after the failure: %q", sent)
			}
		})
	}
}

func jpeg(body string) []byte { return append([]byte{0xff, 0xd8, 0xff, 0xe0}, body...) }

func TestLiveviewYieldsScreenshotsInOrder(t *testing.T) {
	shots := [][]byte{jpeg("one"), jpeg("two"), jpeg("three")}
	p := &phone{shots: slices.Clone(shots)}
	lv, err := NewCamera(p.run).Liveview(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range shots {
		got, err := lv.Next()
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("frame %d = %q, %v; want %q", i, got, err, want)
		}
	}
	p.wantCommands(t, screenshot, screenshot, screenshot)
	if err := lv.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if _, err := lv.Next(); !errors.Is(err, context.Canceled) {
		t.Errorf("Next after Close: err = %v, want context.Canceled", err)
	}
}

func TestLiveviewRejectsOutputThatIsNotAJPEG(t *testing.T) {
	notJPEG := []byte("screencap: unknown option -j\n")
	p := &phone{shots: [][]byte{notJPEG}}
	if lv, err := NewCamera(p.run).Liveview(t.Context()); err == nil || lv != nil || !strings.Contains(err.Error(), "unknown option") {
		t.Errorf("Liveview = %v, %v; want no session and an error quoting the output", lv, err)
	}

	p = &phone{shots: [][]byte{jpeg("one"), notJPEG}}
	lv, err := NewCamera(p.run).Liveview(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lv.Next(); err != nil {
		t.Fatalf("first frame: %v", err)
	}
	if frame, err := lv.Next(); err == nil || !strings.Contains(err.Error(), "unknown option") {
		t.Errorf("second frame = %q, %v; want an error quoting the output", frame, err)
	}
}

func TestLiveviewCancelEndsAScreenshotInFlight(t *testing.T) {
	for name, end := range map[string]func(cancel context.CancelFunc, lv *Liveview){
		"cancel": func(cancel context.CancelFunc, _ *Liveview) { cancel() },
		"close":  func(_ context.CancelFunc, lv *Liveview) { lv.Close() },
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				p := &phone{shots: [][]byte{jpeg("one")}}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				lv, err := NewCamera(p.run).Liveview(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := lv.Next(); err != nil {
					t.Fatalf("first frame: %v", err)
				}
				p.mu.Lock()
				p.hang = true
				p.mu.Unlock()
				done := make(chan error)
				go func() {
					_, err := lv.Next()
					done <- err
				}()
				synctest.Wait() // the screenshot is in flight
				end(cancel, lv)
				if err := <-done; !errors.Is(err, context.Canceled) {
					t.Errorf("Next = %v, want context.Canceled", err)
				}
			})
		})
	}
}

// The package exists so the console can show a Pixel beside the Sony bodies.
func TestConsoleRelaysThePixelsLiveview(t *testing.T) {
	shots := [][]byte{jpeg("one"), jpeg("two"), jpeg("three")}
	p := &phone{shots: slices.Clone(shots)}
	h := console.New([]console.Named{{Name: "pixel9", Camera: console.Adapt(NewCamera(p.run))}})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/pixel9/liveview", nil))
	_, params, err := mime.ParseMediaType(rec.Header().Get("Content-Type"))
	if rec.Code != http.StatusOK || err != nil {
		t.Fatalf("status %d, Content-Type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	parts := multipart.NewReader(rec.Body, params["boundary"])
	for i, want := range shots {
		part, err := parts.NextPart()
		if err != nil {
			t.Fatalf("part %d: %v", i, err)
		}
		if got, _ := io.ReadAll(part); !bytes.Equal(got, want) {
			t.Errorf("part %d = %q, want %q", i, got, want)
		}
	}
}

// fakeADB writes an adb stand-in that logs each invocation's HOME, TMPDIR
// and arguments, and answers the commands the tests use.
func fakeADB(t *testing.T) (path, log string) {
	dir := t.TempDir()
	path, log = filepath.Join(dir, "adb"), filepath.Join(dir, "log")
	script := `#!/bin/sh
echo "$HOME|$TMPDIR|$*" >> '` + log + `'
case "$*" in
"connect phone:5555") echo "already connected to phone:5555" ;;
"connect gone:5555") echo "failed to connect to 'gone:5555': Connection refused" ;;
"-s phone:5555 exec-out screencap -j") printf '\377\330\377\340shot' ;;
"-s phone:5555 shell hang") exec sleep 30 ;;
"-s phone:5555 shell "*) echo "error: device unauthorized." >&2; exit 1 ;;
"-s gone:5555 "*) echo "adb: device 'gone:5555' not found" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, log
}

func TestADB(t *testing.T) {
	path, log := fakeADB(t)
	keys := t.TempDir()

	out, err := ADB(path, keys, "phone:5555")(t.Context(), "exec-out", "screencap", "-j")
	if err != nil || !bytes.Equal(out, jpeg("shot")) {
		t.Errorf("screencap = %q, %v; want the adb output unchanged", out, err)
	}
	logged, _ := os.ReadFile(log)
	want := keys + "|" + keys + "|connect phone:5555\n" + keys + "|" + keys + "|-s phone:5555 exec-out screencap -j\n"
	if string(logged) != want {
		t.Errorf("adb invocations (HOME|TMPDIR|arguments):\n got %q\nwant %q", logged, want)
	}

	_, err = ADB(path, keys, "phone:5555")(t.Context(), "shell", "input keyevent 24")
	if err == nil || !strings.Contains(err.Error(), "device unauthorized") {
		t.Errorf("unauthorized phone: err = %v, want adb's text", err)
	}
	_, err = ADB(path, keys, "gone:5555")(t.Context(), "exec-out", "screencap", "-j")
	if err == nil || !strings.Contains(err.Error(), "Connection refused") || !strings.Contains(err.Error(), "not found") {
		t.Errorf("unreachable phone: err = %v, want adb's text from the connect and the command", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	began := time.Now()
	_, err = ADB(path, keys, "phone:5555")(ctx, "shell", "hang")
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(began) > 5*time.Second {
		t.Errorf("cancelled command: err = %v after %v, want the context's error at once", err, time.Since(began))
	}
}
