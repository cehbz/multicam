package sony

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"slices"
	"sync"
	"syscall"
	"time"
)

// DefaultEndpoint is the camera service URL of the RX10M4 and RX100M6.
const DefaultEndpoint = "http://192.168.122.1:10000/sony/camera"

// Status is a camera's cameraStatus: IDLE, MovieWaitRecStart, MovieRecording,
// MovieWaitRecStop, MovieSaving, NotReady and so on.
type Status string

// StatusIdle is the status a body returns to after a stop, and the only one
// that accepts a start.
const StatusIdle Status = "IDLE"

// State is a camera's state as Watch reports it: its status, and whether its
// liveview can start, which it can while the camera lists startLiveview among
// its available APIs.
type State struct {
	Status   Status
	Liveview bool
}

// Camera is one body, reached at its camera service endpoint. Its connections
// are its own, since both bodies answer at the same address.
type Camera struct {
	// StartGap is how long after the camera's return to IDLE a start may be
	// sent (the RX100M6 ignores a start within 3 s of it). Zero sends at once.
	StartGap time.Duration

	rpc    *Client
	events *Client      // Watch's long polls, held by the camera up to 120 s
	stream *http.Client // liveview streams: no time limit

	cmd sync.Mutex // held while a command is in flight

	mu       sync.Mutex
	watching bool
	idleAt   time.Time // last transition to IDLE seen by Watch; zero if none
}

// NewCamera returns the camera whose camera service is at endpoint, with its
// connections bound to the network interface iface. An empty iface leaves the
// route to the system; a named one needs Linux.
func NewCamera(endpoint, iface string) (*Camera, error) {
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	if iface != "" {
		if bindToDevice == nil {
			return nil, fmt.Errorf("camera on interface %s: binding to an interface is not supported on %s", iface, runtime.GOOS)
		}
		dialer.Control = func(_, _ string, c syscall.RawConn) error {
			var err error
			if cerr := c.Control(func(fd uintptr) { err = bindToDevice(int(fd), iface) }); cerr != nil {
				return cerr
			}
			if err != nil {
				return fmt.Errorf("bind to interface %s: %w", iface, err)
			}
			return nil
		}
	}
	transport := &http.Transport{DialContext: dialer.DialContext}
	rpc := NewClient(endpoint)
	rpc.HTTP.Transport = transport
	events := NewClient(endpoint)
	events.HTTP = &http.Client{Transport: transport, Timeout: 120 * time.Second}
	return &Camera{rpc: rpc, events: events, stream: &http.Client{Transport: transport}}, nil
}

// StartRecording starts movie recording once StartGap has passed since the
// camera's last return to IDLE. Without Watch running it polls the status
// every 100 ms, up to 15 s, until IDLE and times the gap from that read. A
// refusal is the camera's *Error.
func (c *Camera) StartRecording(ctx context.Context) error {
	c.cmd.Lock()
	defer c.cmd.Unlock()
	if err := c.awaitStartGap(ctx); err != nil {
		return fmt.Errorf("startMovieRec: %w", err)
	}
	return c.call(ctx, "startMovieRec")
}

// StopRecording stops movie recording. A refusal is the camera's *Error.
func (c *Camera) StopRecording(ctx context.Context) error {
	c.cmd.Lock()
	defer c.cmd.Unlock()
	return c.call(ctx, "stopMovieRec")
}

func (c *Camera) awaitStartGap(ctx context.Context) error {
	if c.StartGap <= 0 {
		return nil
	}
	c.mu.Lock()
	watching, idleAt := c.watching, c.idleAt
	c.mu.Unlock()
	if !watching {
		var err error
		if idleAt, err = c.awaitIdle(ctx); err != nil {
			return err
		}
	}
	if idleAt.IsZero() {
		return nil
	}
	return sleepUntil(ctx, idleAt.Add(c.StartGap))
}

// awaitIdle polls the status until IDLE and returns when it first read it.
func (c *Camera) awaitIdle(ctx context.Context) (time.Time, error) {
	deadline := time.Now().Add(15 * time.Second)
	for {
		s, err := c.status(ctx)
		if err != nil {
			return time.Time{}, err
		}
		if s == StatusIdle {
			return time.Now(), nil
		}
		if time.Now().After(deadline) {
			return time.Time{}, fmt.Errorf("camera status %s, not IDLE after 15 s", s)
		}
		if err := sleepUntil(ctx, time.Now().Add(100*time.Millisecond)); err != nil {
			return time.Time{}, err
		}
	}
}

func sleepUntil(ctx context.Context, t time.Time) error {
	d := time.Until(t)
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Watch delivers the camera's state, the current one first and then each
// change, until ctx ends or a poll fails and the channel closes. A camera not
// ready yet (Camera Not Ready, as right after a client joins) is read again
// after one long poll, which it answers once it is ready. It holds one long
// poll on its own connection; a poll the camera ends unchanged (Timeout) is
// repeated, and an answer without an API list keeps the last one. One Watch
// per camera: the camera allows one long poll at a time.
func (c *Camera) Watch(ctx context.Context) (<-chan State, error) {
	ev, err := c.event(ctx, false)
	if errorCode(err) == 40401 {
		ev, err = c.whenReady(ctx)
	}
	if err != nil {
		return nil, err
	}
	cur := State{}.update(ev)
	ch := make(chan State, 1)
	ch <- cur
	c.mu.Lock()
	c.watching = true
	c.mu.Unlock()
	go func() {
		defer close(ch)
		defer func() {
			c.mu.Lock()
			c.watching = false
			c.mu.Unlock()
		}()
		for ctx.Err() == nil {
			ev, err := c.event(ctx, true)
			switch {
			case ctx.Err() != nil:
				return
			case errorCode(err) == 2:
				continue
			case err != nil:
				return
			}
			next := cur.update(ev)
			if next == cur {
				continue
			}
			if next.Status == StatusIdle && cur.Status != StatusIdle {
				c.mu.Lock()
				c.idleAt = time.Now()
				c.mu.Unlock()
			}
			cur = next
			select {
			case ch <- cur:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

// whenReady is the getEvent of a camera that was not ready, read once a long
// poll returns or times out.
func (c *Camera) whenReady(ctx context.Context) (*Event, error) {
	if _, err := c.event(ctx, true); err != nil && errorCode(err) != 2 {
		return nil, err
	}
	return c.event(ctx, false)
}

// errorCode is the code of the camera error err carries, 0 when it carries
// none.
func errorCode(err error) int {
	var camErr *Error
	if errors.As(err, &camErr) {
		return camErr.Code
	}
	return 0
}

// update is s with what ev reports: its cameraStatus and its API list, each
// when present.
func (s State) update(ev *Event) State {
	if ev.CameraStatus != "" {
		s.Status = Status(ev.CameraStatus)
	}
	if _, listed := ev.Types["availableApiList"]; listed {
		s.Liveview = slices.Contains(ev.APINames, "startLiveview")
	}
	return s
}

// event is one getEvent.
func (c *Camera) event(ctx context.Context, longPolling bool) (*Event, error) {
	client := c.rpc
	if longPolling {
		client = c.events
	}
	ex, err := client.Call(ctx, "getEvent", "1.3", longPolling)
	if err != nil {
		return nil, fmt.Errorf("getEvent: %w", err)
	}
	return ex.Decoded.Event()
}

// status is the cameraStatus of one getEvent without long polling, empty when
// the answer has none.
func (c *Camera) status(ctx context.Context) (Status, error) {
	ev, err := c.event(ctx, false)
	if err != nil {
		return "", err
	}
	return Status(ev.CameraStatus), nil
}

func (c *Camera) call(ctx context.Context, method string) error {
	if _, err := c.rpc.Call(ctx, method, "1.0"); err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	return nil
}

// Recording reports whether the camera is recording a movie, from one
// getEvent without long polling: cameraStatus MovieRecording, or
// MovieWaitRecStart, which leads to it. MovieWaitRecStop and MovieSaving,
// which follow a stop, are not recording.
func (c *Camera) Recording(ctx context.Context) (bool, error) {
	s, err := c.status(ctx)
	if err != nil {
		return false, err
	}
	return s == "MovieRecording" || s == "MovieWaitRecStart", nil
}

// Liveview is a running liveview session: the camera's stream of JPEG frames,
// from Camera.Liveview until Close.
type Liveview struct {
	cam     *Camera
	ctx     context.Context // done once the session is cancelled or closed
	cancel  context.CancelFunc
	body    io.Closer
	packets *LiveviewReader
}

// Liveview starts the camera's liveview and opens its stream. Cancelling ctx
// ends the stream; Close stops the camera's liveview.
func (c *Camera) Liveview(ctx context.Context) (*Liveview, error) {
	ex, err := c.rpc.Call(ctx, "startLiveview", "1.0")
	if err != nil {
		return nil, fmt.Errorf("startLiveview: %w", err)
	}
	l := &Liveview{cam: c}
	l.ctx, l.cancel = context.WithCancel(ctx)
	if err := l.open(ex.Decoded); err != nil {
		l.cancel()
		l.stop()
		return nil, fmt.Errorf("liveview stream: %w", err)
	}
	return l, nil
}

// open requests the stream startLiveview answered with.
func (l *Liveview) open(start *Response) error {
	u, err := start.FirstString()
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(l.ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := l.cam.stream.Do(req)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return fmt.Errorf("HTTP %s", resp.Status)
	}
	l.body, l.packets = resp.Body, NewLiveviewReader(resp.Body)
	return nil
}

// stop calls stopLiveview; it reaches the camera after the session's context
// is done.
func (l *Liveview) stop() error {
	_, err := l.cam.rpc.Call(context.WithoutCancel(l.ctx), "stopLiveview", "1.0")
	return err
}

// Next returns the next JPEG frame, skipping frame-info packets. It returns
// the context's error once the session is cancelled or closed, and io.EOF
// when the camera ends the stream.
func (l *Liveview) Next() ([]byte, error) {
	for {
		f, err := l.packets.Next()
		if cerr := l.ctx.Err(); cerr != nil {
			return nil, cerr
		}
		if err != nil {
			return nil, err
		}
		if f.Type == PayloadJPEG {
			return f.Data, nil
		}
	}
}

// Close ends the stream and stops the camera's liveview.
func (l *Liveview) Close() error {
	l.cancel()
	l.body.Close()
	return l.stop()
}
