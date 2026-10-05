package blackmagic

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
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

	// active is the active platform's JSON and platforms each custom
	// platform's XML by name, each replaced by a PUT.
	active    string
	platforms map[string]string

	// pushes are record states and streamPushes livestream statuses, each
	// with its own bitrate, delivered on the event socket after the
	// subscribe response in that order; subscribed receives the subscribe
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
	if name, ok := strings.CutPrefix(path, CustomPlatformsPath+"/"); ok {
		f.platform(w, r, name)
		return
	}
	switch call {
	case "POST " + RecordPath, "POST " + StopPath, "PUT " + LivestreamStartPath, "PUT " + LivestreamStopPath:
		w.WriteHeader(http.StatusNoContent)
	case "GET " + ActivePlatformPath:
		f.mu.Lock()
		fmt.Fprint(w, f.active)
		f.mu.Unlock()
	case "PUT " + ActivePlatformPath:
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.active = string(body)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case "GET " + CustomPlatformsPath:
		f.mu.Lock()
		names := slices.Sorted(maps.Keys(f.platforms))
		f.mu.Unlock()
		json.NewEncoder(w).Encode(names)
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

// platform answers a GET of the custom platform name with its XML and
// replaces it with a PUT's body.
func (f *fake) platform(w http.ResponseWriter, r *http.Request, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	xml, ok := f.platforms[name]
	switch {
	case !ok:
		http.NotFound(w, r)
	case r.Method == http.MethodGet:
		fmt.Fprint(w, xml)
	case r.Method == http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		f.platforms[name] = string(body)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method", http.StatusMethodNotAllowed)
	}
}

// events is the notification socket: it announces itself, takes the
// subscribe request, answers it with the current recording and livestream
// status, and pushes each of pushes and streamPushes.
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
	f.mu.Lock()
	recording, livestream := f.recording, f.livestream
	f.mu.Unlock()
	wsjson.Write(ctx, conn, map[string]any{"type": "response", "data": map[string]any{
		"action": "subscribe",
		"values": map[string]any{
			RecordPath:     map[string]any{"recording": recording},
			LivestreamPath: map[string]any{"status": livestream, "bitrate": 0, "effectiveVideoFormat": "1920x1080p25", "duration": 0, "cache": 0.0},
		},
		"properties": []string{RecordPath, LivestreamPath}, "success": true}})
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

// Starts is the number of livestream start calls the fake has had.
func (f *fake) Starts() int {
	n := 0
	for _, c := range f.Calls() {
		if c == "PUT "+LivestreamStartPath {
			n++
		}
	}
	return n
}

// startsReach waits up to 5 s for the fake's start calls to reach n, then
// 100 ms for any more, and returns the count.
func startsReach(f *fake, n int) int {
	for deadline := time.Now().Add(5 * time.Second); f.Starts() < n && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	return f.Starts()
}

// drain discards each state ch delivers.
func drain(ch <-chan State) {
	go func() {
		for range ch {
		}
	}()
}

func TestWatchStartsTheLivestreamWhenItTurnsIdle(t *testing.T) {
	f := newFake(t)
	f.streamPushes = []string{"Connecting", "Streaming", "Idle", "Idle", "Idle", "Streaming"}
	ch, err := NewCamera(f.Address()).Watch(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	drain(ch)
	if n := startsReach(f, 2); n != 2 {
		t.Errorf("livestream started %d times, want 2 (calls %v)", n, f.Calls())
	}
}

// The app answers the subscribe request with the current status and pushes
// nothing more while nothing changes.
func TestWatchStartsAnIdleLivestreamOnSubscribing(t *testing.T) {
	f := newFake(t)
	ch, err := NewCamera(f.Address()).Watch(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	drain(ch)
	if n := startsReach(f, 1); n != 1 {
		t.Errorf("livestream started %d times, want 1 (calls %v)", n, f.Calls())
	}
}

func TestWatchIsStreamingWhenTheLivestreamStreamsOnSubscribing(t *testing.T) {
	f := newFake(t)
	f.recording = true
	f.livestream = StatusStreaming
	ch, err := NewCamera(f.Address()).Watch(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []State{{Recording: true}, {Recording: true, Streaming: true}}
	if got := receive(t, ch, len(want)); !slices.Equal(got, want) {
		t.Errorf("states %v, want %v", got, want)
	}
	quiet(t, ch)
	if n := f.Starts(); n != 0 {
		t.Errorf("a streaming livestream started %d times (calls %v)", n, f.Calls())
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

// activePlatform is the active platform's JSON as the app answers it, with
// url its URL.
func activePlatform(url string) string {
	return `{
    "platform": "Blackmagic Cam App SRT",
    "server": "Custom",
    "key": "",
    "passphrase": "",
    "quality": "MEDIUM",
    "url": "` + url + `"
}`
}

// customPlatform is a custom platform's XML as the app answers it, named
// name, its one server at url, &-escaped.
func customPlatform(name, url string) string {
	return `<?xml version="1.0" encoding="UTF-8"?><streaming>
  <service>
    <name>` + name + `</name>
    <servers>
      <server>
        <name>Pixel 3</name>
        <url>` + strings.ReplaceAll(url, "&", "&amp;") + `</url>
      </server>
    </servers>
    <profiles default="Streaming Medium">
      <profile>
        <name>Streaming Medium</name>
        <config resolution="720p" fps="30" codec="H264">
          <bitrate>2500000</bitrate>
          <audio-bitrate>128000</audio-bitrate>
        </config>
      </profile>
    </profiles>
  </service>
</streaming>
`
}

const (
	staleURL = "srt://192.168.1.116:8890?streamid=publish:pixel9&pkt_size=1316"
	thisURL  = "srt://192.168.1.109:8890?streamid=publish:pixel9&pkt_size=1316"
	otherURL = "srt://192.168.1.50:8890?streamid=publish:other&pkt_size=1316"
)

// pointedFake is an app streaming to staleURL through the active platform
// and the custom platform "Blackmagic Cam App", with another custom platform
// "Other" at otherURL.
func pointedFake(t *testing.T, livestream string) *fake {
	f := newFake(t)
	f.livestream = livestream
	f.active = activePlatform(staleURL)
	f.platforms = map[string]string{
		"Blackmagic Cam App": customPlatform("Blackmagic Cam App", staleURL),
		"Other":              customPlatform("Other", otherURL),
	}
	return f
}

// Active is the active platform's JSON.
func (f *fake) Active() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.active
}

// Platform is the custom platform name's XML.
func (f *fake) Platform(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.platforms[name]
}

// decoded is the JSON text as a map.
func decoded(t *testing.T, text string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		t.Fatalf("%v: %s", err, text)
	}
	return m
}

func TestPointLivestreamRepointsTheActivePlatformAndItsCustomPlatform(t *testing.T) {
	f := pointedFake(t, StatusStreaming)
	if err := NewCamera(f.Address()).PointLivestream(t.Context(), thisURL); err != nil {
		t.Fatal(err)
	}
	if got, want := decoded(t, f.Active()), decoded(t, activePlatform(thisURL)); !reflect.DeepEqual(got, want) {
		t.Errorf("active platform %v, want %v", got, want)
	}
	if got, want := f.Platform("Blackmagic Cam App"), customPlatform("Blackmagic Cam App", thisURL); got != want {
		t.Errorf("custom platform\n%s\nwant\n%s", got, want)
	}
	if got, want := f.Platform("Other"), customPlatform("Other", otherURL); got != want {
		t.Errorf("other custom platform\n%s\nwant\n%s", got, want)
	}
	want := []string{
		"GET /livestreams/0/activePlatform",
		"GET /livestreams/customPlatforms",
		"GET /livestreams/customPlatforms/Blackmagic Cam App",
		"PUT /livestreams/customPlatforms/Blackmagic Cam App",
		"GET /livestreams/customPlatforms/Other",
		"PUT /livestreams/0/activePlatform",
		"GET /livestreams/0",
		"PUT /livestreams/0/stop",
	}
	if got := f.Calls(); !slices.Equal(got, want) {
		t.Errorf("calls\n%v\nwant\n%v", got, want)
	}
}

func TestPointLivestreamChangesNothingWhenItIsPointedAlready(t *testing.T) {
	f := pointedFake(t, StatusStreaming)
	if err := NewCamera(f.Address()).PointLivestream(t.Context(), staleURL); err != nil {
		t.Fatal(err)
	}
	if got, want := f.Calls(), []string{"GET /livestreams/0/activePlatform"}; !slices.Equal(got, want) {
		t.Errorf("calls %v, want %v", got, want)
	}
}

func TestPointLivestreamLeavesAnIdleLivestream(t *testing.T) {
	f := pointedFake(t, StatusIdle)
	if err := NewCamera(f.Address()).PointLivestream(t.Context(), thisURL); err != nil {
		t.Fatal(err)
	}
	if calls := f.Calls(); slices.Contains(calls, "PUT /livestreams/0/stop") {
		t.Errorf("an idle livestream was stopped: calls %v", calls)
	}
}

func TestPointLivestreamFailsWithAFailedCall(t *testing.T) {
	f := pointedFake(t, StatusStreaming)
	f.fail = "PUT /livestreams/0/activePlatform"
	err := NewCamera(f.Address()).PointLivestream(t.Context(), thisURL)
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("PointLivestream = %v, want the call's HTTP 500", err)
	}
	if calls := f.Calls(); slices.Contains(calls, "PUT /livestreams/0/stop") {
		t.Errorf("livestream stopped after a failure: calls %v", calls)
	}
}
