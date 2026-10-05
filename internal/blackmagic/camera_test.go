package blackmagic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// fake is a Blackmagic Camera HTTP server: it records the calls it gets and
// answers from its state.
type fake struct {
	srv *httptest.Server

	mu         sync.Mutex
	calls      []string // "METHOD path", path relative to the API base
	recording  bool
	livestream string // status of /livestreams/0
	fail       string // "METHOD path" answered with 500

	// pushes are record states and streamPushes livestream statuses, each
	// with its own bitrate, delivered on the event socket after the
	// subscribe request in that order; subscribed receives the subscribe
	// request's JSON.
	pushes       []bool
	streamPushes []string
	subscribed   chan json.RawMessage
}

func newFake(t *testing.T) *fake {
	t.Helper()
	f := &fake{livestream: "Idle", subscribed: make(chan json.RawMessage, 1)}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// Address is the host:port the fake listens on.
func (f *fake) Address() string { return strings.TrimPrefix(f.srv.URL, "https://") }

func (f *fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fake) serve(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, BasePath)
	call := r.Method + " " + path
	f.mu.Lock()
	f.calls = append(f.calls, call)
	fail := f.fail == call
	recording, livestream := f.recording, f.livestream
	f.mu.Unlock()
	if fail {
		http.Error(w, "boom", http.StatusInternalServerError)
		return
	}
	switch call {
	case "POST " + RecordPath, "POST " + StopPath, "PUT " + LivestreamStartPath:
		w.WriteHeader(http.StatusNoContent)
	case "GET " + RecordPath:
		fmt.Fprintf(w, `{"recording": %v, "clipName": null}`, recording)
	case "GET " + LivestreamPath:
		fmt.Fprintf(w, `{"status": %q, "bitrate": 0, "effectiveVideoFormat": "1920x1080p24"}`, livestream)
	case "GET " + EventPath:
		f.events(w, r)
	default:
		http.NotFound(w, r)
	}
}

// events is the notification socket: it announces itself, takes the
// subscribe request, acknowledges it and pushes each of pushes.
func (f *fake) events(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	ctx := r.Context()
	wsjson.Write(ctx, conn, map[string]any{"type": "event", "data": map[string]any{"action": "websocketOpened"}})
	_, raw, err := conn.Read(ctx)
	if err != nil {
		return
	}
	f.subscribed <- raw
	wsjson.Write(ctx, conn, map[string]any{"type": "response", "data": map[string]any{
		"action": "subscribe", "properties": []string{RecordPath}, "success": true}})
	for _, v := range f.pushes {
		wsjson.Write(ctx, conn, map[string]any{"type": "event", "data": map[string]any{
			"action": "propertyValueChanged", "property": RecordPath, "value": map[string]any{"recording": v}}})
	}
	for i, s := range f.streamPushes {
		wsjson.Write(ctx, conn, map[string]any{"type": "event", "data": map[string]any{
			"action": "propertyValueChanged", "property": LivestreamPath, "value": map[string]any{"status": s, "bitrate": 1000 * i}}})
	}
	// Hold the socket open until the client goes away.
	conn.Read(ctx)
}

func TestStartRecordingPosts(t *testing.T) {
	f := newFake(t)
	if err := NewCamera(f.Address()).StartRecording(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got, want := f.Calls(), []string{"POST /transports/0/record"}; !slices.Equal(got, want) {
		t.Errorf("calls %v, want %v", got, want)
	}
}

func TestStopRecordingPosts(t *testing.T) {
	f := newFake(t)
	if err := NewCamera(f.Address()).StopRecording(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got, want := f.Calls(), []string{"POST /transports/0/stop"}; !slices.Equal(got, want) {
		t.Errorf("calls %v, want %v", got, want)
	}
}

func TestCommandFailureCarriesStatus(t *testing.T) {
	f := newFake(t)
	f.fail = "POST /transports/0/record"
	err := NewCamera(f.Address()).StartRecording(t.Context())
	if err == nil {
		t.Fatal("StartRecording succeeded on a 500")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error %q does not name the HTTP status", err)
	}
}

func TestRecordingParsesState(t *testing.T) {
	f := newFake(t)
	cam := NewCamera(f.Address())
	for _, want := range []bool{false, true} {
		f.mu.Lock()
		f.recording = want
		f.mu.Unlock()
		got, err := cam.Recording(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("Recording = %v, want %v", got, want)
		}
	}
	if got, want := f.Calls(), []string{"GET /transports/0/record", "GET /transports/0/record"}; !slices.Equal(got, want) {
		t.Errorf("calls %v, want %v", got, want)
	}
}

func TestWatchDeliversRESTStateThenPushes(t *testing.T) {
	f := newFake(t)
	f.recording = true
	f.pushes = []bool{false, true}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ch, err := NewCamera(f.Address()).Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}

	var sub struct {
		Type string
		Data struct {
			Action     string
			Properties []string
		}
	}
	select {
	case raw := <-f.subscribed:
		if err := json.Unmarshal(raw, &sub); err != nil {
			t.Fatalf("subscribe request %s: %v", raw, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no subscribe request")
	}
	if sub.Type != "request" || sub.Data.Action != "subscribe" || !slices.Equal(sub.Data.Properties, []string{"/transports/0/record", "/livestreams/0"}) {
		t.Errorf("subscribe request %+v", sub)
	}

	if got, want := receive(t, ch, 3), []State{{Recording: true}, {}, {Recording: true}}; !slices.Equal(got, want) {
		t.Errorf("states %v, want %v", got, want)
	}

	cancel()
	select {
	case v, ok := <-ch:
		if ok {
			t.Errorf("state %v after cancel, want the channel closed", v)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("channel still open after cancel")
	}
}

// receive is the next n states ch delivers, failing when they don't come in
// 5 s.
func receive(t *testing.T, ch <-chan State, n int) []State {
	t.Helper()
	var got []State
	for range n {
		select {
		case v, ok := <-ch:
			if !ok {
				t.Fatalf("channel closed after %v", got)
			}
			got = append(got, v)
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out after %v", got)
		}
	}
	return got
}

// quiet fails when ch delivers a state within 200 ms.
func quiet(t *testing.T, ch <-chan State) {
	t.Helper()
	select {
	case v, ok := <-ch:
		if ok {
			t.Errorf("state %v delivered, want none", v)
		}
	case <-time.After(200 * time.Millisecond):
	}
}

func TestWatchIsStreamingWhileTheLivestreamIsStreaming(t *testing.T) {
	f := newFake(t)
	f.recording = true
	f.streamPushes = []string{"Streaming", "Idle", "Connecting", "Streaming"}
	ch, err := NewCamera(f.Address()).Watch(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// The first state is read over REST, before any status is pushed.
	want := []State{{Recording: true}, {Recording: true, Streaming: true}, {Recording: true}, {Recording: true, Streaming: true}}
	if got := receive(t, ch, len(want)); !slices.Equal(got, want) {
		t.Errorf("states %v, want %v", got, want)
	}
	quiet(t, ch)
}

// A running stream pushes its bitrate about once a second; a push that
// changes neither the recording state nor streaming is not delivered.
func TestWatchDeliversOnlyChanges(t *testing.T) {
	f := newFake(t)
	f.pushes = []bool{false, true, true}
	f.streamPushes = []string{"Connecting", "Streaming", "Streaming", "Streaming", "Streaming"}
	ch, err := NewCamera(f.Address()).Watch(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []State{{}, {Recording: true}, {Recording: true, Streaming: true}}
	if got := receive(t, ch, len(want)); !slices.Equal(got, want) {
		t.Errorf("states %v, want %v", got, want)
	}
	quiet(t, ch)
}

func TestWatchFailsWhenStateUnreadable(t *testing.T) {
	f := newFake(t)
	f.fail = "GET /transports/0/record"
	if _, err := NewCamera(f.Address()).Watch(t.Context()); err == nil {
		t.Fatal("Watch succeeded without a readable state")
	}
}

func TestWatchStartsTheLivestreamWhenItTurnsIdle(t *testing.T) {
	f := newFake(t)
	f.streamPushes = []string{"Idle", "Connecting", "Streaming", "Idle", "Idle", "Idle", "Streaming"}
	ch, err := NewCamera(f.Address()).Watch(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for range ch {
		}
	}()
	starts := func() int {
		n := 0
		for _, c := range f.Calls() {
			if c == "PUT /livestreams/0/start" {
				n++
			}
		}
		return n
	}
	for deadline := time.Now().Add(5 * time.Second); starts() < 2 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if n := starts(); n != 2 {
		t.Errorf("livestream started %d times, want 2 (calls %v)", n, f.Calls())
	}
}

func TestAdvertisedMatchesTheAppsUniqueID(t *testing.T) {
	txt := []string{"device name=Google Pixel 9 Pro XL", "capabilities=cameraControl", "txtvers=1",
		"unique id=b722b4654dc94e5dbb76a30055bf6a72", "path=/control/api/v1", "camera name=A"}
	if !Advertised("b722b4654dc94e5dbb76a30055bf6a72")(txt) {
		t.Errorf("the app's TXT doesn't match its unique id")
	}
	if Advertised("0000")(txt) {
		t.Errorf("the app's TXT matches another unique id")
	}
}
