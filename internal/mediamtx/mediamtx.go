// Package mediamtx runs MediaMTX as a child of the server for as long as the
// server runs.
package mediamtx

import (
	"context"
	"log/slog"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"time"
)

// Server is a MediaMTX binary to keep running.
type Server struct {
	Path         string        // the executable
	Dir          string        // its working directory, where it finds mediamtx.yml
	Log          string        // the file its output is appended to
	RestartDelay time.Duration // the wait before restarting it after it exits
	StopGrace    time.Duration // the wait between TERM and KILL when ending it

	procDir string // where the processes are listed; "/proc" when empty
}

// Run keeps the server running until ctx is done, then ends it and returns.
// A MediaMTX already running from Path is ended first, so this one owns the
// ports and the log. On Linux the child also dies with its parent.
func (s *Server) Run(ctx context.Context) error {
	// Pdeathsig fires when the thread that started the child exits.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	log, err := os.OpenFile(s.Log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer log.Close()
	s.endRunning()
	for {
		cmd := exec.Command(s.Path)
		cmd.Dir = s.Dir
		cmd.Stdout, cmd.Stderr = log, log
		cmd.SysProcAttr = childAttr()
		if err := cmd.Start(); err != nil {
			slog.Error("mediamtx", "start", err)
		} else {
			slog.Info("mediamtx", "started", cmd.Process.Pid)
			exited := make(chan error, 1)
			go func() { exited <- cmd.Wait() }()
			select {
			case err := <-exited:
				slog.Warn("mediamtx", "exited", err)
			case <-ctx.Done():
				s.end(cmd.Process, exited)
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(s.RestartDelay):
		}
	}
}

// grace is the wait between TERM and KILL.
func (s *Server) grace() time.Duration {
	if s.StopGrace > 0 {
		return s.StopGrace
	}
	return 5 * time.Second
}

// end terminates the child, killing it when it outlasts the grace, and waits
// for exited.
func (s *Server) end(p *os.Process, exited <-chan error) {
	p.Signal(syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(s.grace()):
		p.Kill()
		<-exited
	}
}

// endRunning ends the processes running Path, which an earlier server may
// have left.
func (s *Server) endRunning() {
	dir := s.procDir
	if dir == "" {
		dir = "/proc"
	}
	for _, pid := range running(dir, s.Path) {
		slog.Warn("mediamtx", "ending the one already running", pid)
		syscall.Kill(pid, syscall.SIGTERM)
		deadline := time.Now().Add(s.grace())
		for syscall.Kill(pid, 0) == nil && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if syscall.Kill(pid, 0) == nil {
			syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

// running lists the pids under the process directory proc whose executable
// is path.
func running(proc, path string) []int {
	exes, _ := filepath.Glob(filepath.Join(proc, "[0-9]*", "exe"))
	var pids []int
	for _, exe := range exes {
		target, err := os.Readlink(exe)
		if err != nil || target != path {
			continue
		}
		pid, err := strconv.Atoi(filepath.Base(filepath.Dir(exe)))
		if err == nil && pid != os.Getpid() {
			pids = append(pids, pid)
		}
	}
	return pids
}

// SRTPort is MediaMTX's SRT port, its default.
const SRTPort = 8890

// PublishURL is the SRT URL a camera publishes its stream at, to the MediaMTX
// at addr under path.
func PublishURL(addr netip.Addr, path string) string {
	return "srt://" + netip.AddrPortFrom(addr, SRTPort).String() + "?streamid=publish:" + path + "&pkt_size=1316"
}
