package sony

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// Request is a Camera Remote API JSON-RPC request. Params is always encoded
// as an array, empty when the method takes none.
type Request struct {
	Method  string
	Params  []any
	ID      int
	Version string
}

// NewRequest builds a request.
func NewRequest(id int, method, version string, params ...any) Request {
	if params == nil {
		params = []any{}
	}
	return Request{Method: method, Params: params, ID: id, Version: version}
}

// MarshalJSON encodes the request with its fields in the order Sony's sample
// client sends them.
func (r Request) MarshalJSON() ([]byte, error) {
	params := r.Params
	if params == nil {
		params = []any{}
	}
	return json.Marshal(struct {
		Method  string `json:"method"`
		Params  []any  `json:"params"`
		ID      int    `json:"id"`
		Version string `json:"version"`
	}{r.Method, params, r.ID, r.Version})
}

// Response is a decoded JSON-RPC response. Most methods answer under "result";
// getMethodTypes answers under "results".
type Response struct {
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result"`
	Results json.RawMessage `json:"results"`
	Error   *Error          `json:"error"`
}

// Error is a camera-reported error, encoded on the wire as [code, message].
type Error struct {
	Code    int
	Message string
}

var errorNames = map[int]string{
	1: "Any", 2: "Timeout", 3: "Illegal Argument", 4: "Illegal Data Format",
	5: "Illegal Request", 6: "Illegal Response", 7: "Illegal State",
	8: "Illegal Type", 9: "Index Out Of Bounds", 10: "No Such Element",
	11: "No Such Field", 12: "No Such Method", 13: "Null Pointer",
	14: "Unsupported Version", 15: "Unsupported Operation",
	40400: "Shooting fail", 40401: "Camera Not Ready",
	40402: "Already Running Polling Api", 40403: "Still Capturing Not Finished",
	41003: "Some content could not be deleted",
	401:   "Unauthorized", 403: "Forbidden", 404: "Not Found",
	406: "Not Acceptable", 413: "Request Entity Too Large",
	414: "Request-URI Too Long", 501: "Not Implemented", 503: "Service Unavailable",
}

// Name is the documented name of the error code, empty when undocumented.
func (e *Error) Name() string { return errorNames[e.Code] }

func (e *Error) Error() string {
	s := fmt.Sprintf("camera error %d", e.Code)
	if n := e.Name(); n != "" {
		s += " (" + n + ")"
	}
	if e.Message != "" && e.Message != e.Name() {
		s += ": " + e.Message
	}
	return s
}

// UnmarshalJSON decodes [code] or [code, message].
func (e *Error) UnmarshalJSON(b []byte) error {
	var parts []json.RawMessage
	if err := json.Unmarshal(b, &parts); err != nil {
		return fmt.Errorf("error field: %w", err)
	}
	if len(parts) == 0 {
		return errors.New("error field: empty array")
	}
	if err := json.Unmarshal(parts[0], &e.Code); err != nil {
		return fmt.Errorf("error code: %w", err)
	}
	if len(parts) > 1 {
		json.Unmarshal(parts[1], &e.Message)
	}
	return nil
}

func present(m json.RawMessage) bool { return len(m) > 0 && string(m) != "null" }

// DecodeResponse decodes a response body. A camera-reported error is returned
// as *Error together with the response; a body that is not a JSON-RPC response
// is an ordinary error.
func DecodeResponse(b []byte) (*Response, error) {
	var r Response
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if r.Error != nil {
		return &r, r.Error
	}
	if !present(r.Result) && !present(r.Results) {
		return nil, fmt.Errorf("decode response: no result, results or error: %.200s", b)
	}
	return &r, nil
}

// StringList decodes the [[string, ...]] result of getVersions and
// getAvailableApiList.
func (r *Response) StringList() ([]string, error) {
	var v [][]string
	if err := json.Unmarshal(r.Result, &v); err != nil {
		return nil, fmt.Errorf("string list result: %w", err)
	}
	if len(v) == 0 {
		return nil, errors.New("string list result: empty")
	}
	return v[0], nil
}

// FirstString decodes a result whose first element is a string, such as
// startLiveview's URL.
func (r *Response) FirstString() (string, error) {
	var v []json.RawMessage
	if err := json.Unmarshal(r.Result, &v); err != nil || len(v) == 0 {
		return "", fmt.Errorf("result is not a non-empty array: %.200s", r.Result)
	}
	var s string
	if err := json.Unmarshal(v[0], &s); err != nil {
		return "", fmt.Errorf("result[0] is not a string: %s", v[0])
	}
	return s, nil
}

// MethodType is one getMethodTypes entry.
type MethodType struct {
	Name    string
	Version string
}

// MethodTypes decodes getMethodTypes' results:
// [[name, param types, result types, version], ...].
func (r *Response) MethodTypes() ([]MethodType, error) {
	var rows [][]json.RawMessage
	if err := json.Unmarshal(r.Results, &rows); err != nil {
		return nil, fmt.Errorf("method types: %w", err)
	}
	var out []MethodType
	for _, row := range rows {
		if len(row) < 4 {
			return nil, fmt.Errorf("method types: short row %d", len(row))
		}
		var m MethodType
		if err := json.Unmarshal(row[0], &m.Name); err != nil {
			return nil, fmt.Errorf("method types name: %w", err)
		}
		if err := json.Unmarshal(row[3], &m.Version); err != nil {
			return nil, fmt.Errorf("method types version: %w", err)
		}
		out = append(out, m)
	}
	return out, nil
}

// Event is the subset of a getEvent result the probe reads. Types holds every
// element by its "type", whether it appears as an object or inside an array.
type Event struct {
	CameraStatus string
	ShootMode    string
	APINames     []string
	ZoomPosition *int
	Types        map[string]json.RawMessage
}

// Event decodes a getEvent result. Elements are located by their "type" field
// rather than by index, so version differences in layout don't matter.
func (r *Response) Event() (*Event, error) {
	var elems []json.RawMessage
	if err := json.Unmarshal(r.Result, &elems); err != nil {
		return nil, fmt.Errorf("event: %w", err)
	}
	ev := &Event{Types: map[string]json.RawMessage{}}
	var typed struct {
		Type string `json:"type"`
	}
	add := func(raw json.RawMessage) {
		typed.Type = ""
		if json.Unmarshal(raw, &typed) == nil && typed.Type != "" {
			if _, seen := ev.Types[typed.Type]; !seen {
				ev.Types[typed.Type] = raw
			}
		}
	}
	for _, e := range elems {
		e = bytes.TrimSpace(e)
		if len(e) == 0 {
			continue
		}
		switch e[0] {
		case '{':
			add(e)
		case '[':
			var arr []json.RawMessage
			if json.Unmarshal(e, &arr) == nil {
				for _, a := range arr {
					add(a)
				}
			}
		}
	}
	if raw, ok := ev.Types["availableApiList"]; ok {
		var v struct{ Names []string }
		json.Unmarshal(raw, &v)
		ev.APINames = v.Names
	}
	if raw, ok := ev.Types["cameraStatus"]; ok {
		var v struct{ CameraStatus string }
		json.Unmarshal(raw, &v)
		ev.CameraStatus = v.CameraStatus
	}
	if raw, ok := ev.Types["shootMode"]; ok {
		var v struct{ CurrentShootMode string }
		json.Unmarshal(raw, &v)
		ev.ShootMode = v.CurrentShootMode
	}
	if raw, ok := ev.Types["zoomInformation"]; ok {
		var v struct{ ZoomPosition *int }
		json.Unmarshal(raw, &v)
		ev.ZoomPosition = v.ZoomPosition
	}
	return ev, nil
}

// Exchange is one request/response round trip as sent and received.
// Response and Status are empty when the transport failed; Decoded is nil when
// the body wasn't a JSON-RPC response.
type Exchange struct {
	Request  []byte
	Status   int
	Response []byte
	Decoded  *Response
}

// Client calls one service endpoint, e.g. http://192.168.122.1:8080/sony/camera.
type Client struct {
	Endpoint string
	HTTP     *http.Client

	mu     sync.Mutex
	nextID int
}

// NewClient returns a client for endpoint with a 15 s per-call timeout.
func NewClient(endpoint string) *Client {
	return &Client{Endpoint: endpoint, HTTP: &http.Client{Timeout: 15 * time.Second}}
}

func (c *Client) id() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	return c.nextID
}

// Call posts one request. The returned Exchange is never nil and records
// whatever was sent and received, including on error. A camera-reported error
// is returned as *Error.
func (c *Client) Call(ctx context.Context, method, version string, params ...any) (*Exchange, error) {
	ex := &Exchange{}
	body, err := NewRequest(c.id(), method, version, params...).MarshalJSON()
	if err != nil {
		return ex, err
	}
	ex.Request = body
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(body))
	if err != nil {
		return ex, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return ex, err
	}
	defer resp.Body.Close()
	ex.Status = resp.StatusCode
	ex.Response, err = io.ReadAll(resp.Body)
	if err != nil {
		return ex, fmt.Errorf("%s: read body: %w", method, err)
	}
	if resp.StatusCode != http.StatusOK {
		return ex, fmt.Errorf("%s: HTTP %s", method, resp.Status)
	}
	ex.Decoded, err = DecodeResponse(ex.Response)
	return ex, err
}
