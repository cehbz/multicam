package sony

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"syscall"
	"time"
)

// DefaultEndpoint is the camera service URL of the RX10M4 and RX100M6.
const DefaultEndpoint = "http://192.168.122.1:10000/sony/camera"

// Camera is one body, reached at its camera service endpoint. Its connections
// are its own, since both bodies answer at the same address.
type Camera struct {
	rpc    *Client
	stream *http.Client // liveview streams: no time limit
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
	return &Camera{rpc: rpc, stream: &http.Client{Transport: transport}}, nil
}

// StartRecording starts movie recording. A refusal is the camera's *Error.
func (c *Camera) StartRecording(ctx context.Context) error {
	return c.call(ctx, "startMovieRec")
}

// StopRecording stops movie recording. A refusal is the camera's *Error.
func (c *Camera) StopRecording(ctx context.Context) error {
	return c.call(ctx, "stopMovieRec")
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
	ex, err := c.rpc.Call(ctx, "getEvent", "1.3", false)
	if err != nil {
		return false, fmt.Errorf("getEvent: %w", err)
	}
	ev, err := ex.Decoded.Event()
	if err != nil {
		return false, err
	}
	return ev.CameraStatus == "MovieRecording" || ev.CameraStatus == "MovieWaitRecStart", nil
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
