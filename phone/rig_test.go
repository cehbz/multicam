package phone

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeLinks stands in for links.sh: it records each run and its pid, then
// waits until the test creates links.release.
const fakeLinks = `echo $$ >>"$(dirname "$0")/links.runs"
while [ ! -e "$(dirname "$0")/links.release" ]; do sleep 0.05; done
`

// rigDir is a copy of rig.sh beside a fake links.sh and a one-camera
// links.conf, with a pidof on PATH that finds no daemon.
type rigDir struct {
	t   *testing.T
	dir string
	env []string
}

func newRigDir(t *testing.T) *rigDir {
	t.Helper()
	dir := t.TempDir()
	bin := t.TempDir()
	rig, err := os.ReadFile("rig.sh")
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		filepath.Join(dir, "rig.sh"):     string(rig),
		filepath.Join(dir, "links.sh"):   fakeLinks,
		filepath.Join(dir, "links.conf"): "cam0 CAMERA secret\n",
		filepath.Join(bin, "pidof"):      "#!/bin/sh\nexit 1\n",
	}
	for name, body := range files {
		if err := os.WriteFile(name, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return &rigDir{t, dir, append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))}
}

func (r *rigDir) cmd(args ...string) *exec.Cmd {
	ctx, cancel := context.WithTimeout(r.t.Context(), 30*time.Second)
	r.t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, "sh", append([]string{filepath.Join(r.dir, "rig.sh")}, args...)...)
	cmd.WaitDelay = time.Second
	cmd.Env = r.env
	return cmd
}

func (r *rigDir) path(name string) string { return filepath.Join(r.dir, name) }

// runs is the pids of the links.sh runs so far.
func (r *rigDir) runs() []int {
	b, _ := os.ReadFile(r.path("links.runs"))
	var pids []int
	for f := range strings.FieldsSeq(string(b)) {
		p, err := strconv.Atoi(f)
		if err != nil {
			r.t.Fatal(err)
		}
		pids = append(pids, p)
	}
	return pids
}

// startBlocked starts a start and returns once it is inside links.sh.
func (r *rigDir) startBlocked() *exec.Cmd {
	r.t.Helper()
	start := r.cmd("start")
	if err := start.Start(); err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { start.Process.Kill() })
	for deadline := time.Now().Add(5 * time.Second); len(r.runs()) == 0; {
		if time.Now().After(deadline) {
			r.t.Fatal("the start never reached links.sh")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return start
}

func (r *rigDir) release() {
	if err := os.WriteFile(r.path("links.release"), nil, 0o644); err != nil {
		r.t.Fatal(err)
	}
}

// holdRig writes pid to rig.pid as the rig's owner.
func (r *rigDir) holdRig(pid int) {
	if err := os.WriteFile(r.path("rig.pid"), []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

// stopHolder runs a process whose arguments read like a running stop's.
func stopHolder(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sh", "-c", "sleep 30; :", "rig.sh", "stop")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	return cmd.Process.Pid
}

func (r *rigDir) noOwner() {
	r.t.Helper()
	if _, err := os.Lstat(r.path("rig.pid")); !errors.Is(err, os.ErrNotExist) {
		r.t.Errorf("rig.pid outlived its action: %v", err)
	}
}

func alive(pid int) bool {
	return !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

func TestStartIsRefusedWhileAStartRuns(t *testing.T) {
	r := newRigDir(t)
	first := r.startBlocked()

	out, err := r.cmd("start").CombinedOutput()
	if err == nil {
		t.Error("the second start succeeded")
	}
	if want := "start: already running (pid " + strconv.Itoa(first.Process.Pid) + ")"; !strings.Contains(string(out), want) {
		t.Errorf("output %q; want %q", out, want)
	}
	if n := len(r.runs()); n != 1 {
		t.Errorf("links.sh ran %d times; want 1", n)
	}
	r.release()
	first.Wait()
}

func TestStartIgnoresAnOwnerThatIsNotARigAction(t *testing.T) {
	gone := exec.Command("true")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	holders := map[string]int{
		"dead process":        gone.Process.Pid,
		"process not a start": os.Getpid(),
	}
	for name, pid := range holders {
		t.Run(name, func(t *testing.T) {
			r := newRigDir(t)
			r.holdRig(pid)
			r.release()
			out, _ := r.cmd("start").CombinedOutput()
			if n := len(r.runs()); n != 1 {
				t.Errorf("links.sh ran %d times; want 1 (output %q)", n, out)
			}
			r.noOwner()
		})
	}
}

func TestStopEndsARunningStart(t *testing.T) {
	r := newRigDir(t)
	start := r.startBlocked()
	links := r.runs()[0]

	out, err := r.cmd("stop").CombinedOutput()
	if err != nil {
		t.Errorf("stop: %v (output %q)", err, out)
	}
	if want := "start: ended (pid " + strconv.Itoa(start.Process.Pid) + ")"; !strings.Contains(string(out), want) {
		t.Errorf("output %q; want %q", out, want)
	}
	err = start.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGTERM {
		t.Errorf("start ended with %v; want SIGTERM", err)
	}
	if alive(links) {
		t.Errorf("links.sh (pid %d) outlived the stop", links)
	}
	r.noOwner()
}

func TestStartIsRefusedWhileAStopRuns(t *testing.T) {
	r := newRigDir(t)
	stop := stopHolder(t)
	r.holdRig(stop)
	r.release()

	out, err := r.cmd("start").CombinedOutput()
	if err == nil {
		t.Error("the start succeeded")
	}
	if want := "start: a stop is running (pid " + strconv.Itoa(stop) + ")"; !strings.Contains(string(out), want) {
		t.Errorf("output %q; want %q", out, want)
	}
	if n := len(r.runs()); n != 0 {
		t.Errorf("links.sh ran %d times; want 0", n)
	}
}

func TestStopIsRefusedWhileAStopRuns(t *testing.T) {
	r := newRigDir(t)
	stop := stopHolder(t)
	r.holdRig(stop)

	out, err := r.cmd("stop").CombinedOutput()
	if err == nil {
		t.Error("the second stop succeeded")
	}
	if want := "stop: already running (pid " + strconv.Itoa(stop) + ")"; !strings.Contains(string(out), want) {
		t.Errorf("output %q; want %q", out, want)
	}
}

func TestStopWithNothingRunningSaysSo(t *testing.T) {
	for _, args := range [][]string{{"stop"}, {"stop", "links"}} {
		r := newRigDir(t)
		out, err := r.cmd(args...).CombinedOutput()
		if err == nil {
			t.Errorf("%v succeeded", args)
		}
		if got := strings.TrimSpace(string(out)); got != "nothing to stop" {
			t.Errorf("%v output %q; want %q", args, got, "nothing to stop")
		}
	}
}
