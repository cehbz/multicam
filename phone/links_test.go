package phone

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// keeperFakes are the tools the link keeper meets: wpa_cli (fakeWpaCli), a
// wpa_supplicant that brings its link up DISCONNECTED, and iw, busybox and
// ip, each recording its arguments in the calls file.
var keeperFakes = map[string]string{
	"wpa_cli":        fakeWpaCli,
	"wpa_supplicant": "#!/bin/sh\nIF=${2#-i}\necho \"supplicant $IF\" >>\"$FAKE_WPA/wpa.calls\"\n[ -e \"$FAKE_WPA/wpa.$IF\" ] || echo DISCONNECTED >\"$FAKE_WPA/wpa.$IF\"\n",
	"iw":             "#!/bin/sh\necho \"iw $*\" >>\"$FAKE_WPA/wpa.calls\"\n",
	"busybox":        "#!/bin/sh\necho \"busybox $*\" >>\"$FAKE_WPA/wpa.calls\"\n",
	"ip":             "#!/bin/sh\necho \"ip $*\" >>\"$FAKE_WPA/wpa.calls\"\n",
}

// keeper runs links.sh for one camera, cam0, with its link in state (none
// when empty), until the test ends.
type keeper struct {
	t   *testing.T
	dir string
}

func startKeeper(t *testing.T, state string) *keeper {
	t.Helper()
	t.Parallel()
	dir, bin := t.TempDir(), t.TempDir()
	for name, body := range keeperFakes {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	script, err := os.ReadFile("links.sh")
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"links.sh": string(script), "links.conf": "cam0 CAMERA secret\n"}
	if state != "" {
		files["wpa.cam0"] = state + "\n"
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cmd := exec.CommandContext(ctx, "sh", filepath.Join(dir, "links.sh"))
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "FAKE_WPA="+dir)
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); cmd.Wait() })
	return &keeper{t, dir}
}

// calls is how many recorded calls start with prefix.
func (k *keeper) calls(prefix string) int {
	b, _ := os.ReadFile(filepath.Join(k.dir, "wpa.calls"))
	n := 0
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, prefix) {
			n++
		}
	}
	return n
}

// waitFor waits up to 5 s for n calls starting with prefix.
func (k *keeper) waitFor(prefix string, n int) {
	k.t.Helper()
	for deadline := time.Now().Add(5 * time.Second); k.calls(prefix) < n; {
		if time.Now().After(deadline) {
			b, _ := os.ReadFile(filepath.Join(k.dir, "wpa.calls"))
			k.t.Fatalf("%d calls %q; want %d (calls %q)", k.calls(prefix), prefix, n, b)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (k *keeper) set(state string) {
	if err := os.WriteFile(filepath.Join(k.dir, "wpa.cam0"), []byte(state+"\n"), 0o644); err != nil {
		k.t.Fatal(err)
	}
}

const (
	join  = "cam0 p2p_group_add"
	lease = "busybox udhcpc -i cam0"
	route = "ip route replace 192.168.122.1/32 dev cam0"
)

func TestKeeperStartsASupplicantAndJoins(t *testing.T) {
	k := startKeeper(t, "")
	k.waitFor("supplicant cam0", 1)
	k.waitFor(join, 1)
}

func TestKeeperLeavesAJoiningLinkAlone(t *testing.T) {
	k := startKeeper(t, "SCANNING")
	time.Sleep(2500 * time.Millisecond)
	if n := k.calls(join) + k.calls("supplicant"); n != 0 {
		t.Errorf("%d joins or supplicants for a link already joining", n)
	}
}

func TestKeeperLeasesAJoinedLinkOnce(t *testing.T) {
	k := startKeeper(t, "COMPLETED")
	k.waitFor(lease, 1)
	k.waitFor(route, 1)
	time.Sleep(2500 * time.Millisecond)
	if n, m := k.calls(lease), k.calls(join); n != 1 || m != 0 {
		t.Errorf("%d leases and %d joins for a joined link; want 1 and 0", n, m)
	}
}

func TestKeeperRejoinsALostLink(t *testing.T) {
	k := startKeeper(t, "COMPLETED")
	k.waitFor(lease, 1)
	k.set("DISCONNECTED")
	k.waitFor(join, 1)
	k.set("COMPLETED")
	k.waitFor(lease, 2)
}
