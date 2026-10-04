package link

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv("LINK_FAKE_WPA"); mode != "" {
		fakeWPAMain(mode)
		return
	}
	os.Exit(m.Run())
}

// fakeWPAMain stands in for wpa_supplicant: it prints its arguments and
// library path, then fails as a supplicant on a stuck interface does, or
// serves PING and TERMINATE on its control socket.
func fakeWPAMain(mode string) {
	fmt.Println("args:", strings.Join(os.Args[1:], " "))
	fmt.Println("LD_LIBRARY_PATH=" + os.Getenv("LD_LIBRARY_PATH"))
	if mode == "fail" {
		fmt.Println("nl80211: Could not configure driver mode")
		fmt.Println("wlan1: Failed to initialize driver interface")
		os.Exit(255)
	}
	var iface, dir string
	for i, a := range os.Args[1:] {
		if v, ok := strings.CutPrefix(a, "-i"); ok {
			iface = v
		}
		if a == "-C" {
			dir = os.Args[i+2]
		}
	}
	path := filepath.Join(dir, iface)
	c, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	buf := make([]byte, 4096)
	for {
		n, from, err := c.ReadFromUnix(buf)
		if err != nil {
			os.Exit(1)
		}
		switch string(buf[:n]) {
		case "PING":
			c.WriteToUnix([]byte("PONG\n"), from)
		case "TERMINATE":
			c.WriteToUnix([]byte("OK\n"), from)
			time.Sleep(50 * time.Millisecond)
			os.Remove(path)
			os.Exit(0)
		}
	}
}

// shortDir is a temporary directory whose socket paths fit sun_path.
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "wpa")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// fakeCtrl is a supplicant's control socket answering from replies and
// recording what it was sent.
type fakeCtrl struct {
	c        *net.UnixConn
	mu       sync.Mutex
	got      []string
	attached []*net.UnixAddr
	replies  map[string]string
}

func serveCtrl(t *testing.T, w *WPA, iface string, replies map[string]string) *fakeCtrl {
	t.Helper()
	if err := os.MkdirAll(w.ctrlDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	c, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: filepath.Join(w.ctrlDir(), iface), Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeCtrl{c: c, replies: replies}
	t.Cleanup(func() { c.Close() })
	go func() {
		buf := make([]byte, 4096)
		for {
			n, from, err := c.ReadFromUnix(buf)
			if err != nil {
				return
			}
			cmd := string(buf[:n])
			f.mu.Lock()
			f.got = append(f.got, cmd)
			if cmd == "ATTACH" {
				f.attached = append(f.attached, from)
			}
			r, ok := f.replies[cmd]
			f.mu.Unlock()
			switch {
			case cmd == "PING":
				r = "PONG\n"
			case cmd == "ATTACH":
				r = "OK\n"
			case !ok:
				r = "UNKNOWN COMMAND\n"
			}
			c.WriteToUnix([]byte(r), from)
		}
	}()
	return f
}

func (f *fakeCtrl) push(t *testing.T, ev string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.attached) == 0 {
		t.Fatal("no one attached")
	}
	for _, a := range f.attached {
		f.c.WriteToUnix([]byte(ev), a)
	}
}

func TestDialRequest(t *testing.T) {
	w := &WPA{Dir: shortDir(t)}
	serveCtrl(t, w, "wlan1", map[string]string{"STATUS": "wpa_state=COMPLETED\nssid=DIRECT-ab:DSC-RX10M4\n"})
	c, err := w.Dial("wlan1")
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.Request("STATUS")
	if err != nil {
		t.Fatal(err)
	}
	if want := "wpa_state=COMPLETED\nssid=DIRECT-ab:DSC-RX10M4\n"; r != want {
		t.Errorf("STATUS = %q, want %q", r, want)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if left, _ := os.ReadDir(w.clientDir()); len(left) != 0 {
		t.Errorf("client sockets left: %v", left)
	}
}

func TestDialNone(t *testing.T) {
	w := &WPA{Dir: shortDir(t)}
	if _, err := w.Dial("cam2"); !errors.Is(err, ErrNoSupplicant) {
		t.Errorf("Dial with no socket = %v, want ErrNoSupplicant", err)
	}
	f := serveCtrl(t, w, "cam2", nil)
	f.c.Close() // the socket file stays, as a killed supplicant leaves it
	if _, err := w.Dial("cam2"); !errors.Is(err, ErrNoSupplicant) {
		t.Errorf("Dial with a stale socket = %v, want ErrNoSupplicant", err)
	}
}

func TestAttachEvents(t *testing.T) {
	w := &WPA{Dir: shortDir(t)}
	f := serveCtrl(t, w, "wlan1", nil)
	ev, err := w.Attach("wlan1")
	if err != nil {
		t.Fatal(err)
	}
	defer ev.Close()
	f.push(t, "<3>CTRL-EVENT-CONNECTED - Connection to 76:7a:90:0c:d7:af completed [id=1 id_str=]")
	got, err := ev.Next()
	if err != nil {
		t.Fatal(err)
	}
	if want := "CTRL-EVENT-CONNECTED - Connection to 76:7a:90:0c:d7:af completed [id=1 id_str=]"; got != want {
		t.Errorf("Next = %q, want %q", got, want)
	}
	ev.Close()
	if _, err := ev.Next(); err == nil {
		t.Error("Next after Close succeeded")
	}
}

func TestStartAndStop(t *testing.T) {
	t.Setenv("LINK_FAKE_WPA", "serve")
	w := &WPA{Bin: os.Args[0], Lib: "/data/local/tmp/wifi/lib", Dir: shortDir(t)}
	if err := w.Start(context.Background(), "wlan1"); err != nil {
		t.Fatal(err)
	}
	c, err := w.Dial("wlan1")
	if err != nil {
		t.Fatalf("started supplicant doesn't answer: %v", err)
	}
	c.Close()
	log, _ := os.ReadFile(filepath.Join(w.Dir, "wlan1.log"))
	for _, want := range []string{"-Dnl80211", "-iwlan1", "-C " + w.ctrlDir(), "LD_LIBRARY_PATH=/data/local/tmp/wifi/lib"} {
		if !strings.Contains(string(log), want) {
			t.Errorf("log %q lacks %q", log, want)
		}
	}
	if err := w.Stop(context.Background(), "wlan1"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Dial("wlan1"); !errors.Is(err, ErrNoSupplicant) {
		t.Errorf("Dial after Stop = %v, want ErrNoSupplicant", err)
	}
}

func TestStartFailure(t *testing.T) {
	t.Setenv("LINK_FAKE_WPA", "fail")
	w := &WPA{Bin: os.Args[0], Dir: shortDir(t)}
	err := w.Start(context.Background(), "wlan1")
	if err == nil || !strings.Contains(err.Error(), "Could not configure driver mode") {
		t.Errorf("Start = %v, want the supplicant's failure", err)
	}
}

func TestStopNone(t *testing.T) {
	w := &WPA{Dir: shortDir(t)}
	if err := w.Stop(context.Background(), "cam2"); err != nil {
		t.Errorf("Stop with no supplicant = %v", err)
	}
}

func TestDialSweepsDeadReplySockets(t *testing.T) {
	w := &WPA{Dir: shortDir(t)}
	serveCtrl(t, w, "wlan1", nil)
	if err := os.MkdirAll(w.clientDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	dead, live := filepath.Join(w.clientDir(), "wlan1-1-1"), filepath.Join(w.clientDir(), "wlan1-2-1")
	d, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: dead, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	d.Close() // left behind, as by a killed process
	l, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: live, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	c, err := w.Dial("wlan1")
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if _, err := os.Stat(dead); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("dead reply socket kept: %v", err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Errorf("live reply socket removed: %v", err)
	}
}
