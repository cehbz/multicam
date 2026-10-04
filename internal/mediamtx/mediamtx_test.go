package mediamtx

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// script writes an executable shell script into dir and returns its path.
func script(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "fakemtx")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// waitFor polls cond until it holds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func readFile(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

// alive reports whether pid is a running process.
func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// runAsync runs s until the returned stop is called, which waits for Run.
func runAsync(t *testing.T, s *Server) (stop func()) {
	t.Helper()
	ctx, cancel := contextWithCancel(t)
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	return func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("Run did not return")
		}
	}
}

func TestRunsInItsDirectoryAndLogsItsOutput(t *testing.T) {
	dir := t.TempDir()
	run := filepath.Join(dir, "run")
	os.Mkdir(run, 0o755)
	log := filepath.Join(dir, "mediamtx.log")
	s := &Server{
		Path: script(t, dir, "pwd > ../cwd; echo out; echo err >&2; exec sleep 60\n"),
		Dir:  run, Log: log, RestartDelay: time.Hour,
	}
	stop := runAsync(t, s)
	defer stop()
	waitFor(t, "output in log", func() bool { return strings.Contains(readFile(log), "err") })
	if got, want := readFile(log), "out\nerr\n"; got != want {
		t.Errorf("log = %q, want %q", got, want)
	}
	want, _ := filepath.EvalSymlinks(run)
	got, _ := filepath.EvalSymlinks(strings.TrimSpace(readFile(filepath.Join(dir, "cwd"))))
	if got != want {
		t.Errorf("working directory = %q, want %q", got, want)
	}
}

func TestRestartsAfterItExits(t *testing.T) {
	dir := t.TempDir()
	runs := filepath.Join(dir, "runs")
	s := &Server{
		Path: script(t, dir, "echo x >> "+runs+"\n"),
		Dir:  dir, Log: filepath.Join(dir, "log"), RestartDelay: 10 * time.Millisecond,
	}
	stop := runAsync(t, s)
	defer stop()
	waitFor(t, "three starts", func() bool { return strings.Count(readFile(runs), "x") >= 3 })
}

func TestWaitsTheDelayBeforeRestarting(t *testing.T) {
	dir := t.TempDir()
	runs := filepath.Join(dir, "runs")
	s := &Server{
		Path: script(t, dir, "echo x >> "+runs+"\n"),
		Dir:  dir, Log: filepath.Join(dir, "log"), RestartDelay: time.Hour,
	}
	stop := runAsync(t, s)
	defer stop()
	waitFor(t, "first start", func() bool { return readFile(runs) != "" })
	time.Sleep(100 * time.Millisecond)
	if n := strings.Count(readFile(runs), "x"); n != 1 {
		t.Errorf("starts = %d within the delay, want 1", n)
	}
}

func TestEndsTheChildWhenDone(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	s := &Server{
		Path: script(t, dir, "echo $$ > "+pidFile+"\nexec sleep 60\n"),
		Dir:  dir, Log: filepath.Join(dir, "log"), RestartDelay: 10 * time.Millisecond,
	}
	stop := runAsync(t, s)
	waitFor(t, "pid", func() bool { return strings.HasSuffix(readFile(pidFile), "\n") })
	pid, _ := strconv.Atoi(strings.TrimSpace(readFile(pidFile)))
	stop()
	if alive(pid) {
		syscall.Kill(pid, syscall.SIGKILL)
		t.Errorf("child %d still running after Run returned", pid)
	}
}

func TestKillsAChildThatIgnoresTerm(t *testing.T) {
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	s := &Server{
		Path: script(t, dir, "trap '' TERM\necho up > "+started+"\nwhile :; do sleep 1; done\n"),
		Dir:  dir, Log: filepath.Join(dir, "log"), RestartDelay: time.Hour,
		StopGrace: 100 * time.Millisecond,
	}
	stop := runAsync(t, s)
	waitFor(t, "start", func() bool { return readFile(started) != "" })
	stop()
}

func TestEndsAMediaMTXAlreadyRunning(t *testing.T) {
	dir := t.TempDir()
	path := script(t, dir, "exec sleep 60\n")
	old, err := startSleeper()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { old.Process.Kill(); old.Wait() }()
	proc := t.TempDir()
	for pid, exe := range map[string]string{strconv.Itoa(old.Process.Pid): path, "1": "/sbin/init", "self": path} {
		os.Mkdir(filepath.Join(proc, pid), 0o755)
		os.Symlink(exe, filepath.Join(proc, pid, "exe"))
	}
	gone := make(chan struct{})
	go func() { old.Wait(); close(gone) }()
	s := &Server{
		Path: path, Dir: dir, Log: filepath.Join(dir, "log"), RestartDelay: time.Hour,
		StopGrace: time.Second, procDir: proc,
	}
	stop := runAsync(t, s)
	defer stop()
	select {
	case <-gone:
	case <-time.After(5 * time.Second):
		t.Fatal("the running MediaMTX was not ended")
	}
}

func TestRunFailsWhenTheLogCannotBeOpened(t *testing.T) {
	dir := t.TempDir()
	s := &Server{Path: script(t, dir, "exit 0\n"), Dir: dir, Log: filepath.Join(dir, "no", "log")}
	ctx, cancel := contextWithCancel(t)
	defer cancel()
	if err := s.Run(ctx); err == nil {
		t.Error("Run returned nil, want the log error")
	}
}
