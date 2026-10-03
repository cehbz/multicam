// Package blackmagic drives a phone running Blackmagic Camera through the
// app's HTTP server (Settings → Network Access → Enable HTTP Server).
package blackmagic

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// Port is the HTTP server's port.
const Port = "4444"

// Paths of the REST API, relative to the server; BasePath prefixes the rest.
const (
	BasePath            = "/control/api/v1"
	RecordPath          = "/transports/0/record"
	StopPath            = "/transports/0/stop"
	EventPath           = "/event/websocket"
	LivestreamPath      = "/livestreams/0"
	LivestreamStartPath = "/livestreams/0/start"
)

// StatusIdle is the livestream status when no stream is running; the others
// (Connecting, Streaming, Flushing, Interrupted, Disconnecting) all mean one is.
const StatusIdle = "Idle"

// Camera is one phone, reached at its HTTP server. The server's certificate
// is self-signed, so it is not verified.
type Camera struct {
	address string
	http    *http.Client
}

// NewCamera returns the camera whose HTTP server is at address (host:port).
func NewCamera(address string) *Camera {
	return &Camera{
		address: address,
		http: &http.Client{
			Timeout:   10 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		},
	}
}

func (c *Camera) url(scheme, path string) string {
	return scheme + "://" + c.address + BasePath + path
}

// call sends method to path and returns the body of a 2xx answer. Any other
// answer is an error naming the HTTP status.
func (c *Camera) call(ctx context.Context, method, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.url("https", path), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("%s %s: HTTP %s", method, path, resp.Status)
	}
	return body, nil
}

// StartRecording starts recording a clip.
func (c *Camera) StartRecording(ctx context.Context) error {
	_, err := c.call(ctx, http.MethodPost, RecordPath)
	return err
}

// StopRecording stops recording.
func (c *Camera) StopRecording(ctx context.Context) error {
	_, err := c.call(ctx, http.MethodPost, StopPath)
	return err
}

// record is the record property's value, as the REST answer and the pushed
// event both carry it.
type record struct {
	Recording bool `json:"recording"`
}

// Recording reports whether the camera is recording.
func (c *Camera) Recording(ctx context.Context) (bool, error) {
	body, err := c.call(ctx, http.MethodGet, RecordPath)
	if err != nil {
		return false, err
	}
	var r record
	if err := json.Unmarshal(body, &r); err != nil {
		return false, fmt.Errorf("GET %s: %w", RecordPath, err)
	}
	return r.Recording, nil
}

// eventMessage is one message from the notification socket; only
// propertyValueChanged events carry a value.
type eventMessage struct {
	Type string `json:"type"`
	Data struct {
		Action   string          `json:"action"`
		Property string          `json:"property"`
		Value    json.RawMessage `json:"value"`
	} `json:"data"`
}

// Watch reports the recording state as the camera pushes it: the state read
// over REST first, then each change from the notification socket. The channel
// closes when ctx ends or the socket drops.
func (c *Camera) Watch(ctx context.Context) (<-chan bool, error) {
	conn, _, err := websocket.Dial(ctx, c.url("wss", EventPath), &websocket.DialOptions{HTTPClient: c.http})
	if err != nil {
		return nil, fmt.Errorf("event websocket: %w", err)
	}
	subscribe := map[string]any{"type": "request", "data": map[string]any{
		"action": "subscribe", "properties": []string{RecordPath}}}
	if err := wsjson.Write(ctx, conn, subscribe); err != nil {
		conn.CloseNow()
		return nil, fmt.Errorf("subscribe: %w", err)
	}
	// Read after subscribing so a change between the two is pushed rather
	// than missed.
	current, err := c.Recording(ctx)
	if err != nil {
		conn.CloseNow()
		return nil, err
	}
	ch := make(chan bool)
	go func() {
		defer close(ch)
		defer conn.CloseNow()
		deliver := func(v bool) bool {
			select {
			case ch <- v:
				return true
			case <-ctx.Done():
				return false
			}
		}
		if !deliver(current) {
			return
		}
		for {
			var m eventMessage
			if err := wsjson.Read(ctx, conn, &m); err != nil {
				return
			}
			if m.Type != "event" || m.Data.Action != "propertyValueChanged" || m.Data.Property != RecordPath {
				continue
			}
			var r record
			if err := json.Unmarshal(m.Data.Value, &r); err != nil {
				continue
			}
			if !deliver(r.Recording) {
				return
			}
		}
	}()
	return ch, nil
}

// EnsureStreaming starts the livestream to the configured platform unless
// one is already running.
func (c *Camera) EnsureStreaming(ctx context.Context) error {
	body, err := c.call(ctx, http.MethodGet, LivestreamPath)
	if err != nil {
		return err
	}
	var ls struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &ls); err != nil {
		return fmt.Errorf("GET %s: %w", LivestreamPath, err)
	}
	if ls.Status != StatusIdle {
		return nil
	}
	_, err = c.call(ctx, http.MethodPut, LivestreamStartPath)
	return err
}
