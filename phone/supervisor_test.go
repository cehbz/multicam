package phone

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeServer stands in for multicam. It records "pid cwd args" in starts, then
// takes its behavior from the first line of plan, which it removes: "q" exits
// at once, "l" runs for 2.2 s, "w" waits for the file go. With no line left
// it exits at once.
const fakeServer = `#!/bin/sh
echo "$$ $(pwd) $*" >>"$MC_TEST/starts"
step=$(head -n 1 "$MC_TEST/plan" 2>/dev/null)
sed 1d "$MC_TEST/plan" >"$MC_TEST/plan.new" 2>/dev/null && mv "$MC_TEST/plan.new" "$MC_TEST/plan"
echo "server output"
case $step in
l) sleep 2.2 ;;
w) while [ ! -e "$MC_TEST/go" ]; do sleep 0.02; done ;;
esac
`

// fakeSu runs only `su 2000 -c CMD`; fakeCmd records each notification as
// "tag|title|text".
const (
	fakeSu  = "#!/bin/sh\n[ \"$1\" = 2000 ] && [ \"$2\" = -c ] || exit 9\nexec sh -c \"$3\"\n"
	fakeCmd = "#!/bin/sh\n[ \"$1 $2 $3 $4\" = \"notification post -S bigtext\" ] || exit 9\nprintf '%s|%s|%s\\036' \"$8\" \"$6\" \"$9\" >>\"$MC_TEST/posts\"\n"
)

type supDir struct {
	t   *testing.T
	dir string
	env []string
}

// newSupDir is multicam.sh's environment: a fake server running the plan,
// su and cmd on PATH, a quick threshold of 2 s and short pauses.
func newSupDir(t *testing.T, plan string) *supDir {
	t.Helper()
	dir, bin := t.TempDir(), t.TempDir()
	files := map[string]string{
		filepath.Join(dir, "server"): fakeServer,
		filepath.Join(bin, "su"):     fakeSu,
		filepath.Join(bin, "cmd"):    fakeCmd,
	}
	for name, body := range files {
		if err := os.WriteFile(name, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "plan"), []byte(plan), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &supDir{t, dir, append(os.Environ(),
		"PATH="+bin+":"+os.Getenv("PATH"),
		"MC_TEST="+dir,
		"MC_SERVER="+filepath.Join(dir, "server"),
		"MC_CONFIG="+filepath.Join(dir, "multicam.toml"),
		"MC_DIR="+filepath.Join(dir, "work"),
		"MC_LOG="+filepath.Join(dir, "multicam.log"),
		"MC_HOLD="+filepath.Join(dir, "hold"),
		"MC_PIDFILE="+filepath.Join(dir, "supervisor.pid"),
		"MC_QUICK=2", "MC_RESTART_DELAY=0.05", "MC_POLL=0.05")}
}

func (s *supDir) path(name string) string { return filepath.Join(s.dir, name) }

func (s *supDir) cmd() *exec.Cmd {
	ctx, cancel := context.WithTimeout(s.t.Context(), 30*time.Second)
	s.t.Cleanup(cancel)
	script, err := filepath.Abs("multicam.sh")
	if err != nil {
		s.t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, "sh", script)
	cmd.Env = s.env
	cmd.Dir = s.dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = time.Second
	return cmd
}

// start runs a supervisor in the background; the test ends its process group.
func (s *supDir) start() *exec.Cmd {
	s.t.Helper()
	c := s.cmd()
	if err := c.Start(); err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(func() { syscall.Kill(-c.Process.Pid, syscall.SIGKILL); c.Wait() })
	return c
}

func (s *supDir) read(name string) string {
	b, _ := os.ReadFile(s.path(name))
	return string(b)
}

func (s *supDir) nstarts() int {
	return strings.Count(s.read("starts"), "\n")
}

func (s *supDir) posts() []string {
	var out []string
	for p := range strings.SplitSeq(s.read("posts"), "\x1e") {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (s *supDir) waitStarts(n int) {
	s.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); s.nstarts() < n; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			s.t.Fatalf("only %d starts, want %d", s.nstarts(), n)
		}
	}
}

// exits waits for c to end and reports whether it did.
func exits(c *exec.Cmd, within time.Duration) bool {
	done := make(chan struct{})
	go func() { c.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(within):
		return false
	}
}

func TestSupervisorStartsTheServerAndRestartsItAfterEachExit(t *testing.T) {
	s := newSupDir(t, "")
	s.env = append(s.env, "MC_QUICK=0")
	s.start()
	s.waitStarts(4)
	work := evalLink(t, s.path("work"))
	for l := range strings.SplitSeq(strings.TrimSpace(s.read("starts")), "\n") {
		f := strings.Fields(l)
		if len(f) != 3 || f[2] != s.path("multicam.toml") {
			t.Fatalf("server started as %q, want its config as the only argument", l)
		}
		if evalLink(t, f[1]) != work {
			t.Fatalf("server ran in %q, want %q", f[1], work)
		}
	}
	if got := strings.Count(s.read("multicam.log"), "server output"); got < 3 {
		t.Errorf("log has %d server outputs, want the output of every run appended", got)
	}
}

func evalLink(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestSupervisorStopsAfterFiveQuickExitsAndPostsOnce(t *testing.T) {
	s := newSupDir(t, "")
	sup := s.start()
	if !exits(sup, 10*time.Second) {
		t.Fatal("the supervisor never gave up")
	}
	if n := s.nstarts(); n != 5 {
		t.Errorf("%d starts, want 5", n)
	}
	posts := s.posts()
	if len(posts) != 1 {
		t.Fatalf("%d notifications, want 1: %q", len(posts), posts)
	}
	for _, want := range []string{"multicam|", "crash-looping", s.path("multicam.log")} {
		if !strings.Contains(posts[0], want) {
			t.Errorf("notification %q lacks %q", posts[0], want)
		}
	}
}

func TestSupervisorResetsTheCountAfterALongRun(t *testing.T) {
	s := newSupDir(t, "q\nq\nq\nq\nl\n")
	sup := s.start()
	if !exits(sup, 15*time.Second) {
		t.Fatal("the supervisor never gave up")
	}
	if n := s.nstarts(); n != 10 {
		t.Errorf("%d starts, want 4 quick, a long one and 5 quick", n)
	}
	if n := len(s.posts()); n != 1 {
		t.Errorf("%d notifications, want 1", n)
	}
}

func TestSupervisorDoesNotStartTheServerWhileTheHoldFileExists(t *testing.T) {
	s := newSupDir(t, "")
	s.env = append(s.env, "MC_QUICK=0")
	if err := os.WriteFile(s.path("hold"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s.start()
	time.Sleep(500 * time.Millisecond)
	if n := s.nstarts(); n != 0 {
		t.Fatalf("%d starts under the hold, want 0", n)
	}
	os.Remove(s.path("hold"))
	s.waitStarts(2)
}

func TestSupervisorHoldClearsTheCrashCount(t *testing.T) {
	s := newSupDir(t, "w\n")
	sup := s.start()
	s.waitStarts(1)
	if err := os.WriteFile(s.path("hold"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(s.path("go"), nil, 0o644)
	time.Sleep(500 * time.Millisecond)
	if n := s.nstarts(); n != 1 {
		t.Fatalf("%d starts after the server ended under a hold, want 1", n)
	}
	os.Remove(s.path("hold"))
	if !exits(sup, 10*time.Second) {
		t.Fatal("the supervisor never gave up")
	}
	if n := s.nstarts(); n != 6 {
		t.Errorf("%d starts, want the first and five quick ones after the hold", n)
	}
}

func TestSupervisorSecondStartExitsAndResetsTheCount(t *testing.T) {
	s := newSupDir(t, "q\nq\nq\nq\nw\n")
	first := s.start()
	s.waitStarts(5)
	if err := s.cmd().Run(); err != nil {
		t.Fatalf("second supervisor: %v, want exit 0", err)
	}
	time.Sleep(200 * time.Millisecond)
	os.WriteFile(s.path("go"), nil, 0o644)
	if !exits(first, 10*time.Second) {
		t.Fatal("the first supervisor never gave up")
	}
	if n := s.nstarts(); n != 9 {
		t.Errorf("%d starts, want 5 before the reset and 4 after the waiting server's exit", n)
	}
	if n := len(s.posts()); n != 1 {
		t.Errorf("%d notifications, want 1", n)
	}
}

func TestSupervisorIgnoresAStalePidFile(t *testing.T) {
	s := newSupDir(t, "")
	s.env = append(s.env, "MC_QUICK=0")
	if err := os.WriteFile(s.path("supervisor.pid"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.start()
	s.waitStarts(1)
}
