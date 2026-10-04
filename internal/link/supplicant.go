package link

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Defaults for the wpa_supplicant build on the Pixel 3 XL.
const (
	DefaultSupplicant = "/data/local/tmp/wifi/bin/wpa_supplicant"
	DefaultLib        = "/data/local/tmp/wifi/lib"
)

const (
	requestTimeout = 5 * time.Second
	startTimeout   = 5 * time.Second
	stopTimeout    = 5 * time.Second
	waitStep       = 20 * time.Millisecond
)

// WPA runs a wpa_supplicant on each link's interface and talks to it over its
// control socket. A supplicant runs in a session of its own, so it outlives
// the process that started it.
type WPA struct {
	Bin string // wpa_supplicant
	Lib string // its LD_LIBRARY_PATH; empty leaves the environment's
	// Dir holds the supplicants' control sockets (ctrl/), the reply sockets
	// of connections to them (cli/) and a log per interface (<iface>.log).
	Dir string

	sweep sync.Once
}

func (w *WPA) ctrlDir() string   { return filepath.Join(w.Dir, "ctrl") }
func (w *WPA) clientDir() string { return filepath.Join(w.Dir, "cli") }

// Start starts a wpa_supplicant on iface, configured only by its control
// socket, and returns once it answers. One that exits first is reported with
// the end of its log.
func (w *WPA) Start(ctx context.Context, iface string) error {
	for _, d := range []string{w.ctrlDir(), w.clientDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	logPath := filepath.Join(w.Dir, iface+".log")
	log, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	cmd := exec.Command(w.Bin, "-Dnl80211", "-i"+iface, "-C", w.ctrlDir(), "-t")
	cmd.Env = os.Environ()
	if w.Lib != "" {
		cmd.Env = append(cmd.Env, "LD_LIBRARY_PATH="+w.Lib)
	}
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err = cmd.Start()
	log.Close()
	if err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	timeout := time.NewTimer(startTimeout)
	defer timeout.Stop()
	for {
		if c, err := w.Dial(iface); err == nil {
			c.Close()
			return nil
		}
		select {
		case err := <-exited:
			return fmt.Errorf("wpa_supplicant exited (%v): %s", err, logTail(logPath))
		case <-ctx.Done():
			cmd.Process.Kill()
			return ctx.Err()
		case <-timeout.C:
			cmd.Process.Kill()
			return fmt.Errorf("wpa_supplicant not answering after %v: %s", startTimeout, logTail(logPath))
		case <-time.After(waitStep):
		}
	}
}

// logTail is the last lines of a log.
func logTail(path string) string {
	b, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return strings.Join(lines[max(0, len(lines)-3):], "; ")
}

// Stop ends the wpa_supplicant of iface, if one answers, and returns once its
// control socket has gone.
func (w *WPA) Stop(ctx context.Context, iface string) error {
	c, err := w.Dial(iface)
	if errors.Is(err, ErrNoSupplicant) {
		return nil
	}
	if err != nil {
		return err
	}
	// The supplicant may go before it answers.
	c.Request("TERMINATE")
	c.Close()
	timeout := time.NewTimer(stopTimeout)
	defer timeout.Stop()
	for {
		// A connect, not a PING: a terminating supplicant doesn't answer.
		c, err := w.dial(iface)
		if errors.Is(err, ErrNoSupplicant) {
			return nil
		}
		if err == nil {
			c.Close()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout.C:
			return fmt.Errorf("wpa_supplicant still answers %v after TERMINATE", stopTimeout)
		case <-time.After(waitStep):
		}
	}
}

// Dial connects to the wpa_supplicant of iface and checks that it answers.
func (w *WPA) Dial(iface string) (Control, error) {
	c, err := w.dial(iface)
	if err != nil {
		return nil, err
	}
	if r, err := c.Request("PING"); err != nil || r != "PONG\n" {
		c.Close()
		if err == nil {
			err = fmt.Errorf("PING answered %q", r)
		}
		return nil, err
	}
	return c, nil
}

// Attach subscribes to the events of the wpa_supplicant of iface.
func (w *WPA) Attach(iface string) (Events, error) {
	c, err := w.dial(iface)
	if err != nil {
		return nil, err
	}
	if r, err := c.Request("ATTACH"); err != nil || r != "OK\n" {
		c.Close()
		if err == nil {
			err = fmt.Errorf("ATTACH answered %q", r)
		}
		return nil, err
	}
	c.c.SetReadDeadline(time.Time{})
	return c, nil
}

var clientSeq atomic.Int64

// ctrlConn is a connection to a supplicant's control socket from a reply
// socket of its own.
type ctrlConn struct {
	c     *net.UnixConn
	local string
	once  sync.Once
}

func (w *WPA) dial(iface string) (*ctrlConn, error) {
	local := filepath.Join(w.clientDir(), fmt.Sprintf("%s-%d-%d", iface, os.Getpid(), clientSeq.Add(1)))
	if err := os.MkdirAll(w.clientDir(), 0o700); err != nil {
		return nil, err
	}
	w.sweep.Do(w.sweepReplySockets)
	os.Remove(local)
	c, err := net.DialUnix("unixgram",
		&net.UnixAddr{Name: local, Net: "unixgram"},
		&net.UnixAddr{Name: filepath.Join(w.ctrlDir(), iface), Net: "unixgram"})
	if err != nil {
		os.Remove(local)
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
			return nil, fmt.Errorf("%w on %s", ErrNoSupplicant, iface)
		}
		return nil, err
	}
	return &ctrlConn{c: c, local: local}, nil
}

// sweepReplySockets removes the reply sockets nothing listens on, which a
// killed process leaves behind.
func (w *WPA) sweepReplySockets() {
	ents, _ := os.ReadDir(w.clientDir())
	for _, e := range ents {
		path := filepath.Join(w.clientDir(), e.Name())
		c, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: path, Net: "unixgram"})
		if err == nil {
			c.Close()
			continue
		}
		if errors.Is(err, syscall.ECONNREFUSED) {
			os.Remove(path)
		}
	}
}

// Request sends cmd and returns the answer.
func (c *ctrlConn) Request(cmd string) (string, error) {
	c.c.SetDeadline(time.Now().Add(requestTimeout))
	if _, err := c.c.Write([]byte(cmd)); err != nil {
		if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT) {
			return "", fmt.Errorf("%s: %w", cmd, ErrNoSupplicant)
		}
		return "", fmt.Errorf("%s: %w", cmd, err)
	}
	buf := make([]byte, 4096)
	for {
		n, err := c.c.Read(buf)
		if err != nil {
			return "", fmt.Errorf("%s: %w", cmd, err)
		}
		// An event from an attached connection, not the answer.
		if n > 0 && buf[0] == '<' {
			continue
		}
		return string(buf[:n]), nil
	}
}

// Next returns the next event without its "<level>" prefix.
func (c *ctrlConn) Next() (string, error) {
	buf := make([]byte, 4096)
	n, err := c.c.Read(buf)
	if err != nil {
		return "", err
	}
	ev := buf[:n]
	if len(ev) > 0 && ev[0] == '<' {
		if i := bytes.IndexByte(ev, '>'); i > 0 {
			ev = ev[i+1:]
		}
	}
	return string(ev), nil
}

func (c *ctrlConn) Close() error {
	var err error
	c.once.Do(func() {
		err = c.c.Close()
		os.Remove(c.local)
	})
	return err
}
