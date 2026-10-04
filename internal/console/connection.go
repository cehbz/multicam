package console

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Link is a camera's own network path: brought up before the camera is
// reached and taken down after.
type Link interface {
	// Join brings the link up, adopting it if it is up already: one try.
	Join(ctx context.Context) (Joined, error)
	// Leave takes the link down, whatever its state.
	Leave(ctx context.Context) error
}

// Joined is a link that is up, watched for its loss.
type Joined interface {
	// Lost is closed once the link is lost; Err says why.
	Lost() <-chan struct{}
	Err() error
	// Close ends the watch, leaving the link up.
	Close() error
}

// StateFile is the file that keeps the server's connection state across
// restarts: the user's last Connect or Disconnect. With no file the server is
// Disconnected.
type StateFile string

const (
	savedConnected    = "connected"
	savedDisconnected = "disconnected"
)

// Connected reads whether the saved state is Connected.
func (f StateFile) Connected() (bool, error) {
	b, err := os.ReadFile(string(f))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	switch s := strings.TrimSpace(string(b)); s {
	case savedConnected:
		return true, nil
	case savedDisconnected:
		return false, nil
	default:
		return false, errors.New(string(f) + ": " + s + " is not connected or disconnected")
	}
}

// Save replaces the saved state.
func (f StateFile) Save(connected bool) error {
	s := savedDisconnected
	if connected {
		s = savedConnected
	}
	tmp, err := os.CreateTemp(filepath.Dir(string(f)), filepath.Base(string(f))+".*")
	if err != nil {
		return err
	}
	_, err = tmp.WriteString(s + "\n")
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), string(f))
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}

// connectReport is Connect's answer: the server's state afterwards, why it
// is Disconnected when it is, and every camera's state.
type connectReport struct {
	Connection connState `json:"connection"`
	Error      string    `json:"error,omitempty"`
	Cameras    []state   `json:"cameras"`
}

// disconnectReport is Disconnect's answer: the cameras whose last known
// state was recording, which go on recording.
type disconnectReport struct {
	Connection connState `json:"connection"`
	Recording  []string  `json:"recording"`
}

// leaveTimeout bounds each link's Leave.
const leaveTimeout = 10 * time.Second

// connection is the server's connection to its cameras: Connected or
// Disconnected, as the user last asked and the state file keeps. While
// Connected each camera gets one try to come up (its link joined, if it has
// one, then watched), and one more each time it drops; a camera that does not
// come up stays down until the next Connect. Disconnected, nothing is tried.
type connection struct {
	board *console // where the cameras' states and the server's are shown
	saved StateFile
	base  context.Context // the server's lifetime
	cams  []*cameraConn
	// joins has one slot: links are joined one at a time, since joins at
	// once collide on the radio.
	joins chan struct{}

	mu        sync.Mutex
	connected bool
	session   context.Context // the cameras' connections run under it while Connected
	end       context.CancelFunc
	attempt   *attempt      // the running Connect
	leaving   chan struct{} // closed once the running Disconnect has left the links
}

// cameraConn is one camera's connection.
type cameraConn struct {
	*watched
	phase   connState
	running bool          // its keep runs
	done    chan struct{} // closed when its keep returns
	settled chan struct{} // closed when its try ends; nil when not connecting
	status  Status        // its watch's last, while connected
	feed    *feed         // its relayed picture's feed this connection; nil until started
}

// attempt is a running Connect and, once done is closed, its report.
type attempt struct {
	done   chan struct{}
	report connectReport
}

func newConnection(base context.Context, board *console, saved StateFile) *connection {
	n := &connection{board: board, saved: saved, base: base, joins: make(chan struct{}, 1)}
	for _, w := range board.cameras {
		n.cams = append(n.cams, &cameraConn{watched: w, phase: disconnected})
	}
	return n
}

// restore brings the server to its saved state: Connect when it is
// Connected, otherwise Disconnect, which leaves any link found up.
func (n *connection) restore() {
	connected, err := n.saved.Connected()
	if err != nil {
		slog.Error("connection state", "err", err)
	}
	if connected {
		n.Connect(n.base)
	} else {
		n.Disconnect()
	}
}

// Connect saves Connected and gives each camera not up one try, and reports
// once every try has ended: with no camera up the server goes to
// Disconnected. A Connect while one runs reports that one's outcome; a
// Disconnect ends a running Connect. The Connect runs on when ctx ends,
// without reporting.
func (n *connection) Connect(ctx context.Context) connectReport {
	n.mu.Lock()
	a := n.attempt
	if a == nil {
		a = &attempt{done: make(chan struct{})}
		n.attempt = a
		n.show()
		go func() {
			a.report = n.connect(a)
			close(a.done)
		}()
	}
	n.mu.Unlock()
	select {
	case <-a.done:
		return a.report
	case <-ctx.Done():
		n.mu.Lock()
		defer n.mu.Unlock()
		return n.report(ctx.Err().Error())
	}
}

func (n *connection) connect(a *attempt) connectReport {
	n.mu.Lock()
	for n.leaving != nil && n.attempt == a {
		leaving := n.leaving
		n.mu.Unlock()
		<-leaving
		n.mu.Lock()
	}
	if n.attempt != a {
		return n.ended()
	}
	if err := n.saved.Save(true); err != nil {
		slog.Error("connection state", "err", err)
	}
	n.connected = true
	if n.session == nil {
		n.session, n.end = context.WithCancel(n.base)
	}
	var tries []chan struct{}
	for _, cc := range n.cams {
		if cc.phase == connected {
			continue
		}
		if !cc.running {
			cc.running, cc.done = true, make(chan struct{})
			n.set(cc, state{Connection: connecting})
			go n.keep(n.session, cc)
		}
		if cc.settled != nil {
			tries = append(tries, cc.settled)
		}
	}
	n.show()
	n.mu.Unlock()
	for _, t := range tries {
		<-t
	}

	n.mu.Lock()
	if n.attempt != a {
		return n.ended()
	}
	n.attempt = nil
	if slices.ContainsFunc(n.cams, func(cc *cameraConn) bool { return cc.phase == connected }) {
		defer n.mu.Unlock()
		n.show()
		return n.report("")
	}
	r := n.report("no camera answered")
	_, leave := n.disconnect()
	n.mu.Unlock()
	leave()
	r.Connection = disconnected
	return r
}

// ended is the report of a Connect a Disconnect ended, once the Disconnect
// has left the links. Called with n.mu held, which it releases.
func (n *connection) ended() connectReport {
	if leaving := n.leaving; leaving != nil {
		n.mu.Unlock()
		<-leaving
		n.mu.Lock()
	}
	defer n.mu.Unlock()
	return n.report("ended by Disconnect")
}

// report is Connect's report as things stand; called with n.mu held.
func (n *connection) report(err string) connectReport {
	r := connectReport{Connection: disconnected, Error: err}
	if n.connected {
		r.Connection = connected
	}
	for _, cc := range n.cams {
		r.Cameras = append(r.Cameras, n.board.last(cc.watched))
	}
	return r
}

// Disconnect saves Disconnected, then ends a running Connect and every
// camera's connection and leaves the links. Every camera is shown off, with
// no error. The cameras go on recording; it reports those whose last known
// state is recording.
func (n *connection) Disconnect() disconnectReport {
	n.mu.Lock()
	recording, leave := n.disconnect()
	for _, cc := range n.cams {
		if !cc.running {
			n.set(cc, state{Connection: disconnected})
		}
	}
	n.mu.Unlock()
	leave()
	return disconnectReport{Connection: disconnected, Recording: recording}
}

// disconnect saves Disconnected and ends the session, and returns the
// cameras last known recording and the call, made without n.mu, that waits
// for the cameras' connections to end and leaves the links. Called with n.mu
// held.
func (n *connection) disconnect() ([]string, func()) {
	if err := n.saved.Save(false); err != nil {
		slog.Error("connection state", "err", err)
	}
	n.connected = false
	if n.end != nil {
		n.end()
		n.session, n.end = nil, nil
	}
	n.attempt = nil
	before, leaving := n.leaving, make(chan struct{})
	n.leaving = leaving
	recording := []string{}
	var running []chan struct{}
	for _, cc := range n.cams {
		if n.board.last(cc.watched).recording() {
			recording = append(recording, cc.Name)
		}
		if cc.running {
			running = append(running, cc.done)
		}
	}
	n.show()
	return recording, func() {
		if before != nil {
			<-before
		}
		for _, done := range running {
			<-done
		}
		for _, cc := range n.cams {
			if cc.Link == nil {
				continue
			}
			ctx, cancel := context.WithTimeout(context.WithoutCancel(n.base), leaveTimeout)
			if err := cc.Link.Leave(ctx); err != nil {
				slog.Error("leave", "camera", cc.Name, "err", err)
			}
			cancel()
		}
		n.mu.Lock()
		if n.leaving == leaving {
			n.leaving = nil
		}
		n.mu.Unlock()
		close(leaving)
	}
}

// keep runs one camera's connection for session: a try, then its states
// until it drops, then one more try, and so on until a try fails or the
// session ends.
func (n *connection) keep(session context.Context, cc *cameraConn) {
	var err error
	for {
		var u *upCamera
		if u, err = n.try(session, cc); err != nil {
			break
		}
		err = n.follow(session, cc, u)
		if session.Err() != nil {
			break
		}
		slog.Warn("camera dropped", "camera", cc.Name, "err", err)
		n.mu.Lock()
		n.set(cc, state{Connection: connecting})
		n.mu.Unlock()
	}
	if session.Err() != nil {
		err = nil
	} else {
		slog.Error("connect", "camera", cc.Name, "err", err)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	s := state{Connection: disconnected}
	if err != nil {
		s.Error = err.Error()
	}
	n.set(cc, s)
	cc.running = false
	close(cc.done)
}

// upCamera is a camera that is up: its link, if it has one, and its watch.
type upCamera struct {
	joined Joined             // nil without a link
	states <-chan Status      // the watch's
	live   context.Context    // ends with the camera's connection
	end    context.CancelFunc // ends the watch and live
}

// down ends u's watch, waiting for its channel to close, and closes its
// link's watch, leaving the link up.
func (u *upCamera) down() {
	u.end()
	for range u.states {
	}
	if u.joined != nil {
		u.joined.Close()
	}
}

// errWatchEnded is the drop of a camera whose watch ended.
var errWatchEnded = errors.New("watch ended")

// try brings cc up once: its link joined, one join at a time, then its
// watch started and its first status read.
func (n *connection) try(session context.Context, cc *cameraConn) (*upCamera, error) {
	u := &upCamera{}
	if cc.Link != nil {
		select {
		case n.joins <- struct{}{}:
		case <-session.Done():
			return nil, session.Err()
		}
		joined, err := cc.Link.Join(session)
		<-n.joins
		if err != nil {
			return nil, err
		}
		u.joined = joined
	}
	live, end := context.WithCancel(session)
	u.live, u.end = live, end
	states, err := cc.Watch(live)
	if err != nil {
		end()
		if u.joined != nil {
			u.joined.Close()
		}
		return nil, err
	}
	u.states = states
	select {
	case st, ok := <-states:
		if !ok {
			u.down()
			return nil, errWatchEnded
		}
		n.mu.Lock()
		n.take(cc, u.live, st)
		n.mu.Unlock()
		return u, nil
	case <-session.Done():
		u.down()
		return nil, session.Err()
	}
}

// follow shows u's statuses until it drops, its link lost or its watch ended,
// or the session ends, and returns why, with u down and its feed ended.
func (n *connection) follow(session context.Context, cc *cameraConn, u *upCamera) error {
	lostLink := false
	defer func() {
		n.mu.Lock()
		f := cc.feed
		cc.feed = nil
		n.mu.Unlock()
		if f != nil && lostLink {
			f.abandon()
		}
		u.down()
		if f != nil {
			<-f.ended
		}
	}()
	var lost <-chan struct{}
	if u.joined != nil {
		lost = u.joined.Lost()
	}
	for {
		select {
		case st, ok := <-u.states:
			if !ok {
				return errWatchEnded
			}
			n.mu.Lock()
			n.take(cc, u.live, st)
			n.mu.Unlock()
		case <-lost:
			lostLink = true
			return u.joined.Err()
		case <-session.Done():
			return session.Err()
		}
	}
}

// take shows st, a status of cc's watch, and starts the feed of a relayed
// picture for live, cc's connection, the first time st says its liveview can
// start. Called with n.mu held.
func (n *connection) take(cc *cameraConn, live context.Context, st Status) {
	cc.status = st
	if r, ok := cc.Picture.(Relayed); ok && st.Picture && cc.feed == nil {
		f := newFeed()
		cc.feed = f
		go f.run(live, cc.Name, r.Source, func() {
			n.mu.Lock()
			defer n.mu.Unlock()
			if cc.feed == f {
				n.showUp(cc)
			}
		})
	}
	n.showUp(cc)
}

// showUp shows cc connected with its watch's last status, a relayed picture
// playable while its feed delivers frames. Called with n.mu held.
func (n *connection) showUp(cc *cameraConn) {
	st := cc.status
	if _, ok := cc.Picture.(Relayed); ok {
		st.Picture = cc.feed != nil && cc.feed.delivering()
	}
	n.set(cc, state{Connection: connected, Recording: &st.Recording, Picture: st.Picture})
}

// set makes s cc's state, ending its try when s is not connecting. Called
// with n.mu held.
func (n *connection) set(cc *cameraConn, s state) {
	s.Name = cc.Name
	cc.phase = s.Connection
	switch {
	case s.Connection == connecting && cc.settled == nil:
		cc.settled = make(chan struct{})
	case s.Connection != connecting && cc.settled != nil:
		close(cc.settled)
		cc.settled = nil
	}
	n.board.set(cc.watched, s)
}

// show shows the server's state; called with n.mu held.
func (n *connection) show() {
	s := serverState{Connection: disconnected, Connecting: n.attempt != nil}
	if n.connected {
		s.Connection = connected
	}
	n.board.setServer(s)
}

// frames is a viewer's frames of w's feed, as the feed's frames gives them
// for ctx; nil when the feed isn't running.
func (n *connection) frames(w *watched, ctx context.Context) func() ([]byte, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, cc := range n.cams {
		if cc.watched == w && cc.feed != nil {
			return cc.feed.frames(ctx)
		}
	}
	return nil
}

// wait returns once base has ended and every camera's connection with it.
func (n *connection) wait() {
	<-n.base.Done()
	for {
		n.mu.Lock()
		var running []chan struct{}
		for _, cc := range n.cams {
			if cc.running {
				running = append(running, cc.done)
			}
		}
		n.mu.Unlock()
		if len(running) == 0 {
			return
		}
		for _, done := range running {
			<-done
		}
	}
}
