// Package link keeps the Sony bodies' Wi-Fi Direct links. A link is one
// camera joined as a P2P client on its own interface: the interface, its
// wpa_supplicant, its address lease and its route to the camera. Links are
// found by their state on the supplicant's control socket, so a link outlives
// the process that joined it and is adopted by the next.
package link

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// CameraAddr is where every Sony body answers on its own network.
var CameraAddr = netip.MustParseAddr("192.168.122.1")

// ErrNoSupplicant is the error of reaching an interface no wpa_supplicant
// answers on.
var ErrNoSupplicant = errors.New("no wpa_supplicant answers")

// ErrClosed is a Link's error once Close has ended its watch.
var ErrClosed = errors.New("link watch closed")

// Supplicant events the keeper acts on.
const (
	evConnected        = "CTRL-EVENT-CONNECTED"
	evDisconnected     = "CTRL-EVENT-DISCONNECTED"
	evTerminating      = "CTRL-EVENT-TERMINATING"
	evGroupRemoved     = "P2P-GROUP-REMOVED"
	evFormationFailure = "P2P-GROUP-FORMATION-FAILURE"
)

// Config is one camera's link.
type Config struct {
	Interface string // the phone's interface the camera is joined on
	Network   string // the camera's network name (SSID)
	Password  string
	Table     int // the routing table of the camera's route
}

// Options locate wpa_supplicant, its files and the radio.
type Options struct {
	Supplicant string // wpa_supplicant; DefaultSupplicant if empty
	Lib        string // its LD_LIBRARY_PATH; DefaultLib if empty
	Dir        string // its control sockets, reply sockets and logs
	Phy        string // the radio; phy0 if empty
}

func (o Options) withDefaults() Options {
	if o.Supplicant == "" {
		o.Supplicant = DefaultSupplicant
	}
	if o.Lib == "" {
		o.Lib = DefaultLib
	}
	if o.Phy == "" {
		o.Phy = "phy0"
	}
	return o
}

// Interfaces awaits the radio and creates and deletes its interfaces.
type Interfaces interface {
	// AwaitRadio returns once the radio exists, at once if it does already,
	// or with ctx's error once ctx ends.
	AwaitRadio(ctx context.Context) error
	Exists(name string) (bool, error)
	// Add creates a station interface.
	Add(name string) error
	Delete(name string) error
}

// Supplicants runs the wpa_supplicant of each interface and reaches it on its
// control socket.
type Supplicants interface {
	// Start starts a wpa_supplicant on iface and returns once it answers.
	Start(ctx context.Context, iface string) error
	// Dial connects to the wpa_supplicant of iface: ErrNoSupplicant when none
	// answers.
	Dial(iface string) (Control, error)
	// Attach subscribes to the events of the wpa_supplicant of iface.
	Attach(iface string) (Events, error)
	// Stop ends the wpa_supplicant of iface, if one answers, and returns once
	// it has gone.
	Stop(ctx context.Context, iface string) error
}

// Control is a request connection to a wpa_supplicant.
type Control interface {
	Request(cmd string) (string, error)
	Close() error
}

// Events is a wpa_supplicant's pushed events, without their priority prefix.
type Events interface {
	Next() (string, error)
	Close() error
}

// Network gives links their addresses and routes.
type Network interface {
	// Addr is the interface's IPv4 address; invalid when it has none.
	Addr(iface string) (netip.Prefix, error)
	SetAddr(iface string, p netip.Prefix) error
	// Route routes dst through iface in table, for sockets bound to iface.
	Route(iface string, dst netip.Addr, table int) error
	// Unroute removes what Route added that outlives the interface.
	Unroute(iface string) error
}

// Keeper joins, adopts and leaves links. Calls for different interfaces may
// run at once; calls for one interface are the caller's to order.
type Keeper struct {
	ifaces       Interfaces
	sups         Supplicants
	net          Network
	lease        func(ctx context.Context, iface string) (netip.Prefix, error)
	joinTimeout  time.Duration // from the join request to the join
	leaseTimeout time.Duration
}

func newKeeper(ifaces Interfaces, sups Supplicants, net Network, lease func(context.Context, string) (netip.Prefix, error)) *Keeper {
	return &Keeper{
		ifaces: ifaces, sups: sups, net: net, lease: lease,
		// wpa_supplicant ends a P2P client join after 10 s.
		joinTimeout:  15 * time.Second,
		leaseTimeout: 10 * time.Second,
	}
}

// AwaitRadio returns once the radio the links' interfaces are added to
// exists, or with ctx's error once ctx ends.
func (k *Keeper) AwaitRadio(ctx context.Context) error { return k.ifaces.AwaitRadio(ctx) }

// Join brings up the link of c: a link already joined to c's network is
// adopted; otherwise whatever is on the interface is left and the link joined
// afresh, its address leased and the camera routed through it. A join that
// fails leaves the interface gone.
func (k *Keeper) Join(ctx context.Context, c Config) (*Link, error) {
	l, err := k.Adopt(ctx, c)
	if err != nil || l != nil {
		return l, err
	}
	if err := k.Leave(ctx, c.Interface); err != nil {
		return nil, err
	}
	l, err = k.join(ctx, c)
	if err != nil {
		lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		return nil, errors.Join(err, k.Leave(lctx, c.Interface))
	}
	return l, nil
}

func (k *Keeper) join(ctx context.Context, c Config) (*Link, error) {
	iface := c.Interface
	if err := k.ifaces.Add(iface); err != nil {
		return nil, fmt.Errorf("%s: add interface: %w", iface, err)
	}
	if err := k.sups.Start(ctx, iface); err != nil {
		return nil, fmt.Errorf("%s: start wpa_supplicant: %w", iface, err)
	}
	ctl, err := k.sups.Dial(iface)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", iface, err)
	}
	defer ctl.Close()
	l, err := k.attach(c)
	if err != nil {
		return nil, err
	}
	if err := k.requestJoin(ctl, c); err != nil {
		l.Close()
		return nil, err
	}
	if err := k.awaitJoin(ctx, l); err != nil {
		l.Close()
		return nil, err
	}
	if err := k.connect(ctx, l); err != nil {
		l.Close()
		return nil, err
	}
	go l.watch()
	return l, nil
}

// requestJoin gives the supplicant c's network as a persistent P2P client
// profile and starts a client join with it.
func (k *Keeper) requestJoin(ctl Control, c Config) error {
	id, err := ctl.Request("ADD_NETWORK")
	if err != nil {
		return fmt.Errorf("%s: add network: %w", c.Interface, err)
	}
	id = strings.TrimSpace(id)
	for _, set := range []string{
		`ssid "` + c.Network + `"`,
		`psk "` + c.Password + `"`,
		"proto RSN",
		"key_mgmt WPA-PSK",
		"pairwise CCMP",
		"mode 0",
		"disabled 2",
	} {
		if err := expectOK(ctl, "SET_NETWORK "+id+" "+set); err != nil {
			field, _, _ := strings.Cut(set, " ")
			return fmt.Errorf("%s: set network %s: %w", c.Interface, field, err)
		}
	}
	if err := expectOK(ctl, "SET p2p_no_group_iface 1"); err != nil {
		return fmt.Errorf("%s: %w", c.Interface, err)
	}
	if err := expectOK(ctl, "P2P_GROUP_ADD persistent="+id); err != nil {
		return fmt.Errorf("%s: join: %w", c.Interface, err)
	}
	return nil
}

func expectOK(ctl Control, cmd string) error {
	r, err := ctl.Request(cmd)
	if err != nil {
		return err
	}
	if r = strings.TrimSpace(r); r != "OK" {
		return fmt.Errorf("answered %q", r)
	}
	return nil
}

// awaitJoin waits for the join to complete, fail or time out.
func (k *Keeper) awaitJoin(ctx context.Context, l *Link) error {
	timer := time.NewTimer(k.joinTimeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s: join: %w", l.Interface, ctx.Err())
		case <-timer.C:
			return fmt.Errorf("%s: not joined to %s within %v", l.Interface, l.Network, k.joinTimeout)
		case ev, ok := <-l.events:
			if !ok {
				return fmt.Errorf("%s: join: %w", l.Interface, l.readErr)
			}
			switch {
			case strings.HasPrefix(ev, evConnected):
				return nil
			case strings.HasPrefix(ev, evFormationFailure), strings.HasPrefix(ev, evGroupRemoved), strings.HasPrefix(ev, evTerminating):
				return fmt.Errorf("%s: join to %s failed: %s", l.Interface, l.Network, ev)
			}
		}
	}
}

// Adopt takes over the link of c if its supplicant reports it joined to c's
// network, without disturbing the join: the address is leased only if the
// interface has none, and the route is put in place. With no such link it
// returns nil.
func (k *Keeper) Adopt(ctx context.Context, c Config) (*Link, error) {
	ctl, err := k.sups.Dial(c.Interface)
	if errors.Is(err, ErrNoSupplicant) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", c.Interface, err)
	}
	defer ctl.Close()
	// Attached before the status read, so a change after it is seen.
	l, err := k.attach(c)
	if err != nil {
		return nil, err
	}
	st, err := ctl.Request("STATUS")
	if err != nil {
		l.Close()
		return nil, fmt.Errorf("%s: status: %w", c.Interface, err)
	}
	status := parseStatus(st)
	if status["wpa_state"] != "COMPLETED" || status["ssid"] != c.Network {
		l.Close()
		return nil, nil
	}
	l.Adopted = true
	if err := k.connect(ctx, l); err != nil {
		l.Close()
		return nil, err
	}
	go l.watch()
	return l, nil
}

func parseStatus(s string) map[string]string {
	m := map[string]string{}
	for line := range strings.Lines(s) {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			m[k] = v
		}
	}
	return m
}

// connect leases the link's address if its interface has none and routes the
// camera through it.
func (k *Keeper) connect(ctx context.Context, l *Link) error {
	iface := l.Interface
	addr, err := k.net.Addr(iface)
	if err != nil {
		return fmt.Errorf("%s: address: %w", iface, err)
	}
	if !addr.IsValid() {
		lctx, cancel := context.WithTimeout(ctx, k.leaseTimeout)
		addr, err = k.lease(lctx, iface)
		cancel()
		if err != nil {
			return fmt.Errorf("%s: lease: %w", iface, err)
		}
		if err := k.net.SetAddr(iface, addr); err != nil {
			return fmt.Errorf("%s: set address %v: %w", iface, addr, err)
		}
	}
	if err := k.net.Route(iface, CameraAddr, l.Table); err != nil {
		return fmt.Errorf("%s: route: %w", iface, err)
	}
	l.Addr = addr
	return nil
}

// Leave ends the link on iface, whatever its state: its supplicant is ended
// and the interface deleted, since a supplicant ended after a join leaves the
// interface in a mode the next supplicant can't set up.
func (k *Keeper) Leave(ctx context.Context, iface string) error {
	var errs []error
	if err := k.sups.Stop(ctx, iface); err != nil {
		errs = append(errs, fmt.Errorf("%s: stop wpa_supplicant: %w", iface, err))
	}
	if ok, err := k.ifaces.Exists(iface); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", iface, err))
	} else if ok {
		if err := k.ifaces.Delete(iface); err != nil {
			errs = append(errs, fmt.Errorf("%s: delete interface: %w", iface, err))
		}
	}
	if err := k.net.Unroute(iface); err != nil {
		errs = append(errs, fmt.Errorf("%s: unroute: %w", iface, err))
	}
	return errors.Join(errs...)
}

// attach starts reading the supplicant's events for a link of c.
func (k *Keeper) attach(c Config) (*Link, error) {
	ev, err := k.sups.Attach(c.Interface)
	if err != nil {
		return nil, fmt.Errorf("%s: attach: %w", c.Interface, err)
	}
	l := &Link{Config: c, ev: ev, events: make(chan string, 64), done: make(chan struct{}), lost: make(chan struct{})}
	go l.read()
	return l, nil
}

// Link is a joined camera link, watched for its loss.
type Link struct {
	Config
	Addr    netip.Prefix // the interface's address
	Adopted bool         // joined before this process found it

	ev      Events
	events  chan string // ev's lines; closed on a read error, which is readErr
	readErr error
	done    chan struct{} // closed by Close

	lost   chan struct{}
	mu     sync.Mutex
	err    error
	closed bool
}

func (l *Link) read() {
	for {
		ev, err := l.ev.Next()
		if err != nil {
			l.readErr = err
			close(l.events)
			return
		}
		select {
		case l.events <- ev:
		case <-l.done:
			return
		}
	}
}

// watch reports the link lost on the supplicant's first event saying so.
func (l *Link) watch() {
	for {
		select {
		case <-l.done:
			l.end(ErrClosed)
			return
		case ev, ok := <-l.events:
			if !ok {
				l.end(fmt.Errorf("%s: events: %w", l.Interface, l.readErr))
				return
			}
			if strings.HasPrefix(ev, evDisconnected) || strings.HasPrefix(ev, evGroupRemoved) || strings.HasPrefix(ev, evTerminating) {
				l.end(fmt.Errorf("%s: lost %s: %s", l.Interface, l.Network, ev))
				l.ev.Close()
				return
			}
		}
	}
}

func (l *Link) end(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return
	}
	if l.closed {
		err = ErrClosed
	}
	l.err = err
	close(l.lost)
}

// Lost is closed once the link is lost or its watch closed; Err says which.
func (l *Link) Lost() <-chan struct{} { return l.lost }

// Err is why Lost was closed; nil while it is open.
func (l *Link) Err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

// Close ends the watch, leaving the link as it is.
func (l *Link) Close() error {
	l.mu.Lock()
	if !l.closed {
		l.closed = true
		close(l.done)
	}
	l.mu.Unlock()
	return l.ev.Close()
}
