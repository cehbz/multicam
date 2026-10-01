// Package pixel drives a Pixel phone as a camera over adb: its picture is its
// screen, and recording is Pixel Camera's shutter.
package pixel

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Run runs one adb command against the phone and returns its output.
type Run func(ctx context.Context, args ...string) ([]byte, error)

// ADB returns the Run that executes the adb binary at path against the phone
// at address. adb's HOME and TMPDIR are keyDir, where it keeps its key. Each
// command follows a connect to address, which does nothing once connected.
func ADB(path, keyDir, address string) Run {
	return func(ctx context.Context, args ...string) ([]byte, error) {
		adb := func(args ...string) ([]byte, error) {
			cmd := exec.CommandContext(ctx, path, args...)
			cmd.Env = append(os.Environ(), "HOME="+keyDir, "TMPDIR="+keyDir)
			cmd.WaitDelay = time.Second
			return cmd.Output()
		}
		connected, _ := adb("connect", address)
		out, err := adb(append([]string{"-s", address}, args...)...)
		if err == nil {
			return out, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		text := err.Error()
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(bytes.TrimSpace(exit.Stderr)) > 0 {
			text = string(bytes.TrimSpace(exit.Stderr))
		}
		return nil, fmt.Errorf("adb %s: %s (connect: %s)", strings.Join(args, " "), text, bytes.TrimSpace(connected))
	}
}

const (
	cameraApp = "com.google.android.GoogleCamera"
	// Each filter runs on the phone, so the rest of its dump stays there.
	statusCommand = "dumpsys audio | grep 'source client=CAMCORDER' || true"
	frontCommand  = "dumpsys activity activities | grep 'topResumedActivity=.*" + cameraApp + "/' || true"
	launchCommand = "am start -W -a android.media.action.VIDEO_CAMERA -p " + cameraApp
	// Volume up: Pixel Camera's shutter, the phone's volume anywhere else.
	shutterCommand = "input keyevent 24"

	confirmWithin = 5 * time.Second
	confirmEvery  = 500 * time.Millisecond
)

// Camera is a Pixel phone running Pixel Camera.
type Camera struct {
	run Run
}

// NewCamera returns the camera reached through run.
func NewCamera(run Run) *Camera { return &Camera{run: run} }

// Recording reports whether the phone has an active camcorder audio source.
func (c *Camera) Recording(ctx context.Context) (bool, error) {
	out, err := c.run(ctx, "shell", statusCommand)
	if err != nil {
		return false, err
	}
	return bytes.Contains(out, []byte("source client=CAMCORDER")), nil
}

// StartRecording starts a recording unless one is running: it brings Pixel
// Camera to the front, presses its shutter and waits for the recording.
func (c *Camera) StartRecording(ctx context.Context) error {
	return c.setRecording(ctx, true, "recording did not start: check that Pixel Camera is in Video mode")
}

// StopRecording stops the recording if one is running, the same way.
func (c *Camera) StopRecording(ctx context.Context) error {
	return c.setRecording(ctx, false, "recording did not stop: check Pixel Camera on the phone")
}

func (c *Camera) setRecording(ctx context.Context, want bool, unchanged string) error {
	recording, err := c.Recording(ctx)
	if err != nil || recording == want {
		return err
	}
	front, err := c.inFront(ctx)
	if err != nil {
		return err
	}
	if !front {
		// The launch reports success on a locked phone too; only the
		// front-app check says where the shutter key would land.
		if _, err := c.run(ctx, "shell", launchCommand); err != nil {
			return err
		}
		if front, err = c.inFront(ctx); err != nil {
			return err
		}
		if !front {
			return errors.New("Pixel Camera is not in front: unlock the phone and open Pixel Camera in Video mode")
		}
	}
	if _, err := c.run(ctx, "shell", shutterCommand); err != nil {
		return err
	}
	deadline := time.Now().Add(confirmWithin)
	for {
		recording, err := c.Recording(ctx)
		if err != nil || recording == want {
			return err
		}
		if !time.Now().Before(deadline) {
			return errors.New(unchanged)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(confirmEvery):
		}
	}
}

// inFront reports whether Pixel Camera is the phone's top resumed activity.
func (c *Camera) inFront(ctx context.Context) (bool, error) {
	out, err := c.run(ctx, "shell", frontCommand)
	if err != nil {
		return false, err
	}
	return bytes.Contains(out, []byte("topResumedActivity=")) && bytes.Contains(out, []byte(" "+cameraApp+"/")), nil
}

// Liveview is a running series of screenshots, each taken when asked for.
type Liveview struct {
	cam    *Camera
	ctx    context.Context // done once the session is cancelled or closed
	cancel context.CancelFunc
	first  []byte // the screenshot taken at the start, until Next hands it out
}

// Liveview starts a session with a first screenshot. Cancelling ctx or
// closing the session ends a screenshot in flight.
func (c *Camera) Liveview(ctx context.Context) (*Liveview, error) {
	l := &Liveview{cam: c}
	l.ctx, l.cancel = context.WithCancel(ctx)
	first, err := l.screenshot()
	if err != nil {
		l.cancel()
		return nil, err
	}
	l.first = first
	return l, nil
}

// Next returns the next screenshot as a JPEG. It returns the context's error
// once the session is cancelled or closed.
func (l *Liveview) Next() ([]byte, error) {
	if err := l.ctx.Err(); err != nil {
		return nil, err
	}
	if first := l.first; first != nil {
		l.first = nil
		return first, nil
	}
	return l.screenshot()
}

func (l *Liveview) screenshot() ([]byte, error) {
	out, err := l.cam.run(l.ctx, "exec-out", "screencap", "-j")
	if cerr := l.ctx.Err(); cerr != nil {
		return nil, cerr
	}
	if err != nil {
		return nil, err
	}
	if !bytes.HasPrefix(out, []byte{0xff, 0xd8, 0xff}) {
		return nil, fmt.Errorf("screencap: not a JPEG: %.100q", out)
	}
	return out, nil
}

// Close ends the session.
func (l *Liveview) Close() error {
	l.cancel()
	return nil
}
