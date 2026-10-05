package link

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

var rx10 = Config{Interface: "wlan1", Network: "DIRECT-ab:DSC-RX10M4", Password: "secret12", Table: 2001}

// fake is the phone's radio, supplicants and network stack as the keeper
// sees them, recording each operation in order.
type fake struct {
	mu       sync.Mutex
	ops      []string
	ifaces   map[string]bool
	sups     map[string]*fakeSup
	addrs    map[string]netip.Prefix
	rules    map[string]int // interface → routing table
	routes   map[string]netip.Addr
	lease    netip.Prefix
	leaseErr error
	join     []string // the events a P2P_GROUP_ADD brings
}

type fakeSup struct {
	state, ssid string
	watchers    []*fakeEvents
}

func newFake() *fake {
	return &fake{
		ifaces: map[string]bool{},
		sups:   map[string]*fakeSup{},
		addrs:  map[string]netip.Prefix{},
		rules:  map[string]int{},
		routes: map[string]netip.Addr{},
		lease:  netip.MustParsePrefix("192.168.0.8/16"),
		join:   []string{"CTRL-EVENT-CONNECTED - Connection to 76:7a:90:0c:d7:af completed [id=1 id_str=]"},
	}
}

func (f *fake) keeper() *Keeper {
	k := newKeeper(f, f, f, f.leaseFn)
	k.joinTimeout = time.Second
	return k
}

func (f *fake) record(op string) { f.ops = append(f.ops, op) }

func (f *fake) opsSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.ops)
}

// emit pushes an event to the attached watchers of iface's supplicant.
// Called with f.mu held.
func (f *fake) emit(iface, ev string) {
	s := f.sups[iface]
	if s == nil {
		return
	}
	for _, w := range s.watchers {
		select {
		case <-w.closed:
		case w.ch <- ev:
		}
	}
}

func (f *fake) Exists(name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ifaces[name], nil
}

func (f *fake) AwaitRadio(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("await radio")
	return nil
}

func (f *fake) Add(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("add " + name)
	if f.ifaces[name] {
		return fmt.Errorf("%s exists", name)
	}
	f.ifaces[name] = true
	return nil
}

func (f *fake) Delete(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("delete " + name)
	if !f.ifaces[name] {
		return fmt.Errorf("no %s", name)
	}
	delete(f.ifaces, name)
	delete(f.addrs, name)
	delete(f.routes, name)
	return nil
}

func (f *fake) Start(_ context.Context, iface string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("start " + iface)
	if !f.ifaces[iface] {
		return fmt.Errorf("no %s", iface)
	}
	if f.sups[iface] != nil {
		return fmt.Errorf("%s already has a supplicant", iface)
	}
	f.sups[iface] = &fakeSup{state: "DISCONNECTED"}
	return nil
}

func (f *fake) Dial(iface string) (Control, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sups[iface] == nil {
		return nil, ErrNoSupplicant
	}
	return &fakeControl{f, iface}, nil
}

func (f *fake) Attach(iface string) (Events, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.sups[iface]
	if s == nil {
		return nil, ErrNoSupplicant
	}
	w := &fakeEvents{ch: make(chan string, 16), closed: make(chan struct{})}
	s.watchers = append(s.watchers, w)
	return w, nil
}

func (f *fake) Stop(_ context.Context, iface string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("stop " + iface)
	f.emit(iface, "CTRL-EVENT-TERMINATING")
	delete(f.sups, iface)
	return nil
}

func (f *fake) Addr(iface string) (netip.Prefix, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addrs[iface], nil
}

func (f *fake) SetAddr(iface string, p netip.Prefix) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("addr " + iface + " " + p.String())
	f.addrs[iface] = p
	return nil
}

func (f *fake) Route(iface string, dst netip.Addr, table int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(fmt.Sprintf("route %s %s %d", iface, dst, table))
	f.routes[iface] = dst
	f.rules[iface] = table
	return nil
}

func (f *fake) Unroute(iface string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("unroute " + iface)
	delete(f.rules, iface)
	return nil
}

func (f *fake) leaseFn(_ context.Context, iface string) (netip.Prefix, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("lease " + iface)
	return f.lease, f.leaseErr
}

type fakeControl struct {
	f     *fake
	iface string
}

func (c *fakeControl) Request(cmd string) (string, error) {
	f := c.f
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.sups[c.iface]
	if s == nil {
		return "", ErrNoSupplicant
	}
	f.record(c.iface + ": " + cmd)
	switch {
	case cmd == "STATUS":
		return fmt.Sprintf("bssid=76:7a:90:0c:d7:af\nssid=%s\nwpa_state=%s\naddress=3c:28:6d:cd:b2:85\n", s.ssid, s.state), nil
	case cmd == "ADD_NETWORK":
		return "0\n", nil
	case strings.HasPrefix(cmd, "SET"):
		return "OK\n", nil
	case strings.HasPrefix(cmd, "P2P_GROUP_ADD"):
		for _, ev := range f.join {
			if strings.HasPrefix(ev, "CTRL-EVENT-CONNECTED") {
				s.state, s.ssid = "COMPLETED", rx10.Network
			}
			f.emit(c.iface, ev)
		}
		return "OK\n", nil
	}
	return "UNKNOWN COMMAND\n", nil
}

func (c *fakeControl) Close() error { return nil }

type fakeEvents struct {
	ch     chan string
	closed chan struct{}
	once   sync.Once
}

func (e *fakeEvents) Next() (string, error) {
	select {
	case ev := <-e.ch:
		return ev, nil
	case <-e.closed:
		return "", errors.New("closed")
	}
}

func (e *fakeEvents) Close() error {
	e.once.Do(func() { close(e.closed) })
	return nil
}

// joined puts a supplicant on iface already joined to network.
func (f *fake) joined(iface, network string) {
	f.ifaces[iface] = true
	f.sups[iface] = &fakeSup{state: "COMPLETED", ssid: network}
}

// assertLeft checks that nothing of the link on iface remains.
func (f *fake) assertLeft(t *testing.T, iface string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sups[iface] != nil {
		t.Errorf("%s still has a supplicant", iface)
	}
	if f.ifaces[iface] {
		t.Errorf("%s still exists", iface)
	}
	if _, ok := f.rules[iface]; ok {
		t.Errorf("%s still has a rule", iface)
	}
}

// assertOrder checks that want appear in ops in this order.
func assertOrder(t *testing.T, ops, want []string) {
	t.Helper()
	i := 0
	for _, op := range ops {
		if i < len(want) && op == want[i] {
			i++
		}
	}
	if i < len(want) {
		t.Errorf("ops %q: missing %q in order %q", ops, want[i], want)
	}
}

func assertNone(t *testing.T, ops []string, prefixes ...string) {
	t.Helper()
	for _, op := range ops {
		for _, p := range prefixes {
			if strings.HasPrefix(op, p) {
				t.Errorf("unexpected op %q", op)
			}
		}
	}
}

func TestKeeperAwaitsItsRadio(t *testing.T) {
	f := newFake()
	if err := f.keeper().AwaitRadio(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := f.opsSnapshot(); !slices.Equal(got, []string{"await radio"}) {
		t.Errorf("ops %v, want the radio awaited", got)
	}
}

func TestJoinFromNothing(t *testing.T) {
	f := newFake()
	l, err := f.keeper().Join(context.Background(), rx10)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if l.Adopted {
		t.Error("a fresh join reported as adopted")
	}
	if l.Addr != f.lease {
		t.Errorf("Addr = %v, want %v", l.Addr, f.lease)
	}
	assertOrder(t, f.opsSnapshot(), []string{
		"add wlan1",
		"start wlan1",
		"wlan1: ADD_NETWORK",
		`wlan1: SET_NETWORK 0 ssid "DIRECT-ab:DSC-RX10M4"`,
		`wlan1: SET_NETWORK 0 psk "secret12"`,
		"wlan1: SET_NETWORK 0 mode 0",
		"wlan1: SET_NETWORK 0 disabled 2",
		"wlan1: P2P_GROUP_ADD persistent=0",
		"lease wlan1",
		"addr wlan1 192.168.0.8/16",
		"route wlan1 192.168.122.1 2001",
	})
	if f.addrs["wlan1"] != f.lease || f.rules["wlan1"] != 2001 || f.routes["wlan1"] != CameraAddr {
		t.Errorf("addrs %v rules %v routes %v", f.addrs, f.rules, f.routes)
	}
}

func TestJoinRecreatesStaleInterface(t *testing.T) {
	f := newFake()
	f.ifaces["wlan1"] = true
	l, err := f.keeper().Join(context.Background(), rx10)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	assertOrder(t, f.opsSnapshot(), []string{"delete wlan1", "add wlan1", "start wlan1"})
}

func TestJoinRestartsUnjoinedSupplicant(t *testing.T) {
	f := newFake()
	f.ifaces["wlan1"] = true
	f.sups["wlan1"] = &fakeSup{state: "SCANNING"}
	l, err := f.keeper().Join(context.Background(), rx10)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	assertOrder(t, f.opsSnapshot(), []string{"stop wlan1", "delete wlan1", "add wlan1", "start wlan1", "wlan1: P2P_GROUP_ADD persistent=0"})
}

func TestJoinAdoptsJoinedLink(t *testing.T) {
	f := newFake()
	f.joined("wlan1", rx10.Network)
	l, err := f.keeper().Join(context.Background(), rx10)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if !l.Adopted {
		t.Error("not reported as adopted")
	}
	ops := f.opsSnapshot()
	assertNone(t, ops, "stop ", "delete ", "add ", "start ", "wlan1: P2P_GROUP_ADD", "wlan1: SET")
	assertOrder(t, ops, []string{"lease wlan1", "addr wlan1 192.168.0.8/16", "route wlan1 192.168.122.1 2001"})
}

func TestAdoptKeepsAddress(t *testing.T) {
	f := newFake()
	f.joined("wlan1", rx10.Network)
	had := netip.MustParsePrefix("192.168.0.15/16")
	f.addrs["wlan1"] = had
	l, err := f.keeper().Adopt(context.Background(), rx10)
	if err != nil {
		t.Fatal(err)
	}
	if l == nil {
		t.Fatal("joined link not adopted")
	}
	defer l.Close()
	if l.Addr != had {
		t.Errorf("Addr = %v, want %v", l.Addr, had)
	}
	ops := f.opsSnapshot()
	assertNone(t, ops, "lease ", "addr ")
	assertOrder(t, ops, []string{"route wlan1 192.168.122.1 2001"})
}

func TestAdoptNone(t *testing.T) {
	for name, setup := range map[string]func(*fake){
		"no supplicant":   func(*fake) {},
		"not joined":      func(f *fake) { f.ifaces["wlan1"] = true; f.sups["wlan1"] = &fakeSup{state: "SCANNING"} },
		"another network": func(f *fake) { f.joined("wlan1", "DIRECT-zz:other") },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake()
			setup(f)
			l, err := f.keeper().Adopt(context.Background(), rx10)
			if err != nil || l != nil {
				t.Fatalf("Adopt = %v, %v; want nil, nil", l, err)
			}
			assertNone(t, f.opsSnapshot(), "stop ", "delete ", "lease ", "route ")
		})
	}
}

func TestJoinFailureLeaves(t *testing.T) {
	f := newFake()
	f.join = []string{"CTRL-EVENT-SCAN-RESULTS", "P2P-GROUP-FORMATION-FAILURE"}
	l, err := f.keeper().Join(context.Background(), rx10)
	if err == nil || !strings.Contains(err.Error(), "P2P-GROUP-FORMATION-FAILURE") {
		t.Fatalf("Join = %v, %v; want the formation failure", l, err)
	}
	f.assertLeft(t, "wlan1")
}

func TestJoinTimeoutLeaves(t *testing.T) {
	f := newFake()
	f.join = []string{"CTRL-EVENT-DISCONNECTED bssid=00:00:00:00:00:00 reason=3"}
	k := f.keeper()
	k.joinTimeout = 50 * time.Millisecond
	start := time.Now()
	if _, err := k.Join(context.Background(), rx10); err == nil {
		t.Fatal("Join succeeded with no join")
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("Join took %v", d)
	}
	f.assertLeft(t, "wlan1")
}

func TestJoinCanceledLeaves(t *testing.T) {
	f := newFake()
	f.join = nil
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := f.keeper().Join(ctx, rx10); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Join = %v, want the context's error", err)
	}
	f.assertLeft(t, "wlan1")
}

func TestLeaseFailureLeaves(t *testing.T) {
	f := newFake()
	f.leaseErr = errors.New("no offer")
	if _, err := f.keeper().Join(context.Background(), rx10); err == nil || !strings.Contains(err.Error(), "no offer") {
		t.Fatalf("Join = %v, want the lease failure", err)
	}
	f.assertLeft(t, "wlan1")
}

func TestLostOnDisconnect(t *testing.T) {
	f := newFake()
	l, err := f.keeper().Join(context.Background(), rx10)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	f.mu.Lock()
	f.emit("wlan1", "CTRL-EVENT-SCAN-RESULTS")
	f.mu.Unlock()
	select {
	case <-l.Lost():
		t.Fatalf("lost on a scan: %v", l.Err())
	case <-time.After(50 * time.Millisecond):
	}
	f.mu.Lock()
	f.emit("wlan1", "CTRL-EVENT-DISCONNECTED bssid=76:7a:90:0c:d7:af reason=3 locally_generated=1")
	f.mu.Unlock()
	select {
	case <-l.Lost():
	case <-time.After(time.Second):
		t.Fatal("drop not reported")
	}
	if err := l.Err(); err == nil || !strings.Contains(err.Error(), "CTRL-EVENT-DISCONNECTED") {
		t.Errorf("Err = %v, want the disconnect", err)
	}
}

func TestLeave(t *testing.T) {
	f := newFake()
	k := f.keeper()
	l, err := k.Join(context.Background(), rx10)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := k.Leave(context.Background(), "wlan1"); err != nil {
		t.Fatal(err)
	}
	f.assertLeft(t, "wlan1")
	select {
	case <-l.Lost():
	case <-time.After(time.Second):
		t.Fatal("leaving not reported as lost")
	}
}

func TestLeaveNothing(t *testing.T) {
	f := newFake()
	if err := f.keeper().Leave(context.Background(), "cam2"); err != nil {
		t.Fatal(err)
	}
	assertNone(t, f.opsSnapshot(), "delete ")
}

func TestCloseEndsWatch(t *testing.T) {
	f := newFake()
	l, err := f.keeper().Join(context.Background(), rx10)
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	select {
	case <-l.Lost():
	case <-time.After(time.Second):
		t.Fatal("Lost still open after Close")
	}
	if !errors.Is(l.Err(), ErrClosed) {
		t.Errorf("Err = %v, want ErrClosed", l.Err())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sups["wlan1"] == nil || f.sups["wlan1"].state != "COMPLETED" {
		t.Error("Close disturbed the link")
	}
}
