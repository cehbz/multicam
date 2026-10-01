package sony

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// DefaultEndpoint is the camera service URL of the RX10M4 and RX100M6.
const DefaultEndpoint = "http://192.168.122.1:10000/sony/camera"

// Camera is one body, reached at its camera service endpoint.
type Camera struct {
	rpc *Client
}

// NewCamera returns the camera whose camera service is at endpoint.
func NewCamera(endpoint string) *Camera {
	return &Camera{rpc: NewClient(endpoint)}
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
	resp, err := http.DefaultClient.Do(req)
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
