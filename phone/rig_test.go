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

// fakeLinks stands in for the link keeper: it records its pid in links.runs
// and runs until it is ended.
const fakeLinks = `#!/bin/sh
echo $$ >>"$(dirname "$0")/links.runs"
while :; do sleep 0.05; done
`

// fakePidof finds no daemon. Called from a start while $RIG_HOLD exists, it
// records its pid in $RIG_HOLD.runs and waits until the file is gone, which
// holds the start before it starts anything.
const fakePidof = `#!/bin/sh
case $(ps -o args= -p $PPID) in
*"rig.sh start"*)
	if [ -e "$RIG_HOLD" ]; then
		echo $$ >>"$RIG_HOLD.runs"
		while [ -e "$RIG_HOLD" ]; do sleep 0.05; done
	fi
	;;
esac
exit 1
`

// rigDir is a copy of rig.sh beside a fake link keeper and a one-camera
// links.conf, with fakes of pidof, wpa_cli and ip on PATH.
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
		filepath.Join(bin, "pidof"):      fakePidof,
		filepath.Join(bin, "wpa_cli"):    fakeWpaCli,
		filepath.Join(bin, "ip"):         "#!/bin/sh\n",
	}
	for name, body := range files {
		if err := os.WriteFile(name, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	r := &rigDir{t, dir, append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "FAKE_WPA="+dir, "RIG_HOLD="+filepath.Join(dir, "hold"))}
	t.Cleanup(func() {
		for _, p := range r.runs() {
			syscall.Kill(p, syscall.SIGKILL)
		}
	})
	return r
}

// fakeWpaCli answers `wpa_cli -p DIR -i IF CMD` from the file wpa.IF in
// $FAKE_WPA, which holds the link's state while its supplicant runs, and
// records each command in wpa.calls.
const fakeWpaCli = `#!/bin/sh
echo "$4 $5" >>"$FAKE_WPA/wpa.calls"
case $5 in
status) [ -e "$FAKE_WPA/wpa.$4" ] && echo "wpa_state=$(cat "$FAKE_WPA/wpa.$4")" ;;
terminate) rm -f "$FAKE_WPA/wpa.$4" ;;
esac
`

func (r *rigDir) cmd(args ...string) *exec.Cmd {
	ctx, cancel := context.WithTimeout(r.t.Context(), 30*time.Second)
	r.t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, "sh", append([]string{filepath.Join(r.dir, "rig.sh")}, args...)...)
	cmd.WaitDelay = time.Second
	cmd.Env = r.env
	return cmd
}

func (r *rigDir) path(name string) string { return filepath.Join(r.dir, name) }

// runs is the pids of the link keepers started so far.
func (r *rigDir) runs() []int { return r.pids("links.runs") }

// held is the pids of the fake pidofs holding a start.
func (r *rigDir) held() []int { return r.pids("hold.runs") }

func (r *rigDir) pids(name string) []int {
	b, _ := os.ReadFile(r.path(name))
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

// startBlocked starts a start and returns once it is held, before it has
// started anything.
func (r *rigDir) startBlocked() *exec.Cmd {
	r.t.Helper()
	if err := os.WriteFile(r.path("hold"), nil, 0o644); err != nil {
		r.t.Fatal(err)
	}
	start := r.cmd("start")
	if err := start.Start(); err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { start.Process.Kill() })
	for deadline := time.Now().Add(5 * time.Second); len(r.held()) == 0; {
		if time.Now().After(deadline) {
			r.t.Fatal("the start was never held")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return start
}

// release lets a held start go on.
func (r *rigDir) release() {
	if err := os.Remove(r.path("hold")); err != nil && !errors.Is(err, os.ErrNotExist) {
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
	if n := len(r.runs()); n != 0 {
		t.Errorf("the refused start launched %d keepers", n)
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
			out, _ := r.cmd("start").CombinedOutput()
			if n := len(r.runs()); n != 1 {
				t.Errorf("start launched %d keepers; want 1 (output %q)", n, out)
			}
			r.noOwner()
		})
	}
}

func TestStopEndsARunningStart(t *testing.T) {
	r := newRigDir(t)
	start := r.startBlocked()
	holder := r.held()[0]

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
	if alive(holder) {
		t.Errorf("the start's child (pid %d) outlived the stop", holder)
	}
	r.noOwner()
}

func TestStartIsRefusedWhileAStopRuns(t *testing.T) {
	r := newRigDir(t)
	stop := stopHolder(t)
	r.holdRig(stop)

	out, err := r.cmd("start").CombinedOutput()
	if err == nil {
		t.Error("the start succeeded")
	}
	if want := "start: a stop is running (pid " + strconv.Itoa(stop) + ")"; !strings.Contains(string(out), want) {
		t.Errorf("output %q; want %q", out, want)
	}
	if n := len(r.runs()); n != 0 {
		t.Errorf("the refused start launched %d keepers", n)
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
	r := newRigDir(t)
	out, err := r.cmd("stop").CombinedOutput()
	if err == nil {
		t.Error("stop succeeded")
	}
	if got := strings.TrimSpace(string(out)); got != "nothing to stop" {
		t.Errorf("output %q; want %q", got, "nothing to stop")
	}
}

func TestStopEndsTheLinks(t *testing.T) {
	r := newRigDir(t)
	if err := os.WriteFile(r.path("wpa.cam0"), []byte("COMPLETED\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := r.cmd("stop").CombinedOutput()
	if err != nil {
		t.Errorf("stop: %v (output %q)", err, out)
	}
	if !strings.Contains(string(out), "cam0: left CAMERA") {
		t.Errorf("output %q; want cam0 left", out)
	}
	if _, err := os.Stat(r.path("wpa.cam0")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("cam0's supplicant still runs: %v", err)
	}
}

func TestStartLaunchesTheKeeperAfterTheDaemonsAndReturns(t *testing.T) {
	r := newRigDir(t)
	out, _ := r.cmd("start").CombinedOutput()
	keepers := r.runs()
	if len(keepers) != 1 {
		t.Fatalf("start launched %d keepers; want 1 (output %q)", len(keepers), out)
	}
	if !alive(keepers[0]) {
		t.Error("the keeper ended with the start")
	}
	daemons, keeper := strings.Index(string(out), "multicam:"), strings.Index(string(out), "links: running")
	if daemons < 0 || keeper < daemons {
		t.Errorf("output %q; want the daemons, then the keeper running", out)
	}
	if status, _ := r.cmd("status").CombinedOutput(); !strings.Contains(string(status), "links: running") {
		t.Errorf("status %q does not report the keeper", status)
	}
}

func TestStopEndsTheKeeperBeforeTheLinks(t *testing.T) {
	r := newRigDir(t)
	r.cmd("start").Run()
	if err := os.WriteFile(r.path("wpa.cam0"), []byte("COMPLETED\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := r.cmd("stop").CombinedOutput()
	if err != nil {
		t.Errorf("stop: %v (output %q)", err, out)
	}
	for _, p := range r.runs() {
		if alive(p) {
			t.Errorf("the keeper (pid %d) outlived the stop", p)
		}
	}
	keeper, link := strings.Index(string(out), "links: stopped"), strings.Index(string(out), "cam0: left")
	if keeper < 0 || link < keeper {
		t.Errorf("output %q; want the keeper stopped, then the link left", out)
	}
}
