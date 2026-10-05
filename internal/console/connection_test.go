package console

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cehbz/multicam/internal/sony/sonytest"
)

func TestStateFileKeepsTheLastConnectOrDisconnect(t *testing.T) {
	f := StateFile(filepath.Join(t.TempDir(), "connection.state"))
	if connected, err := f.Connected(); connected || err != nil {
		t.Fatalf("with nothing saved: %v, %v; want Disconnected", connected, err)
	}
	for _, want := range []bool{true, false, true} {
		if err := f.Save(want); err != nil {
			t.Fatal(err)
		}
		if got, err := f.Connected(); got != want || err != nil {
			t.Errorf("after Save(%v): %v, %v", want, got, err)
		}
	}
	if err := os.WriteFile(string(f), []byte("maybe\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if connected, err := f.Connected(); connected || err == nil {
		t.Errorf("unreadable state: %v, %v; want Disconnected and an error", connected, err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(string(f))); len(entries) != 1 {
		t.Errorf("directory holds %d files, want the state file alone", len(entries))
	}
}

// fakeLink is a camera's link. Each Join waits for hold, when set, and then
// fails with the next of errs or joins (nil errs, or past their end); Leave
// is counted.
type fakeLink struct {
	radio   *radio
	hold    chan struct{}
	errs    []error
	onLeave func()

	mu         sync.Mutex
	joins      int
	leaves     int
	joined     []*fakeJoined
	inJoin     bool
	leftInJoin bool // a Leave came while a Join ran
}

// radio counts the joins in flight of the links sharing it.
type radio struct{ inFlight, most atomic.Int32 }

func (l *fakeLink) Join(ctx context.Context) (Joined, error) {
	if l.radio != nil {
		n := l.radio.inFlight.Add(1)
		defer l.radio.inFlight.Add(-1)
		for m := l.radio.most.Load(); n > m && !l.radio.most.CompareAndSwap(m, n); m = l.radio.most.Load() {
		}
	}
	l.mu.Lock()
	l.joins++
	i := l.joins
	l.inJoin = true
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		l.inJoin = false
		l.mu.Unlock()
	}()
	if l.hold != nil {
		select {
		case <-l.hold:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if i <= len(l.errs) && l.errs[i-1] != nil {
		return nil, l.errs[i-1]
	}
	j := &fakeJoined{lost: make(chan struct{})}
	l.joined = append(l.joined, j)
	return j, nil
}

func (l *fakeLink) Leave(context.Context) error {
	if l.onLeave != nil {
		l.onLeave()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.leaves++
	if l.inJoin {
		l.leftInJoin = true
	}
	return nil
}

func (l *fakeLink) counts() (joins, leaves int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.joins, l.leaves
}

// last is the link's last join.
func (l *fakeLink) last() *fakeJoined {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.joined[len(l.joined)-1]
}

type fakeJoined struct {
	lost   chan struct{}
	err    error
	closed atomic.Bool
}

func (j *fakeJoined) Lost() <-chan struct{} { return j.lost }
func (j *fakeJoined) Err() error            { return j.err }
func (j *fakeJoined) Close() error {
	j.closed.Store(true)
	return nil
}

// lose reports the link lost with err.
func (j *fakeJoined) lose(err error) {
	j.err = err
	close(j.lost)
}

// linked returns a camera under name with a link of its own, its state fed by
// the returned script.
func linked(name string) (Named, *scripted, *fakeLink) {
	cam, f := fed(name)
	l := &fakeLink{}
	cam.Link = l
	return cam, f, l
}

// post sends a POST of target in the background; the channel delivers the
// answer.
func post(console http.Handler, target string) <-chan *httptest.ResponseRecorder {
	answer := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		console.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, target, nil))
		answer <- rec
	}()
	return answer
}

// decode decodes a command's JSON answer into v.
func decode(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); rec.Code != http.StatusOK || ct != "application/json" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status %d, Content-Type %q, Cache-Control %q; want 200 application/json no-store: %s",
			rec.Code, ct, rec.Header().Get("Cache-Control"), rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("%v: %s", err, rec.Body)
	}
}

func connectAnswer(t *testing.T, answer <-chan *httptest.ResponseRecorder) connectReport {
	t.Helper()
	var r connectReport
	decode(t, <-answer, &r)
	return r
}

func connect(t *testing.T, console http.Handler) connectReport {
	t.Helper()
	return connectAnswer(t, post(console, "/connect"))
}

func disconnect(t *testing.T, console http.Handler) disconnectReport {
	t.Helper()
	var r disconnectReport
	decode(t, <-post(console, "/disconnect"), &r)
	return r
}

// look is what a viewer connecting now is sent, in the test's bubble: the
// server's state and a summary of the cameras'.
func look(t *testing.T, console http.Handler) (serverState, string) {
	t.Helper()
	events := stream(t, console)
	synctest.Wait()
	var server serverState
	data, ok := strings.CutPrefix(waiting(t, events), "connection ")
	if !ok {
		t.Fatalf("first event %q, want the connection", data)
	}
	if err := json.Unmarshal([]byte(data), &server); err != nil {
		t.Fatal(err)
	}
	var states []state
	for len(events) > 0 {
		var s state
		if err := json.Unmarshal([]byte(<-events), &s); err != nil {
			t.Fatal(err)
		}
		states = append(states, s)
	}
	return server, summary(states)
}

var (
	isDisconnected = serverState{Connection: disconnected}
	isConnected    = serverState{Connection: connected}
)

func TestNothingSavedStartsDisconnectedAndLeavesTheLinks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		body, bf, bl := linked("body")
		phone, pf := fed("phone")
		console := New(t.Context(), []Named{body, phone}, StateFile(filepath.Join(t.TempDir(), "connection.state")))
		synctest.Wait()
		if joins, leaves := bl.counts(); joins != 0 || leaves != 1 {
			t.Errorf("link: %d joins, %d leaves; want none joined and the link left", joins, leaves)
		}
		if n := bf.watches.Load() + pf.watches.Load(); n != 0 {
			t.Errorf("%d watches, want none", n)
		}
		if server, cams := look(t, console); server != isDisconnected || cams != "body=off phone=off" {
			t.Errorf("server %+v, cameras %q; want Disconnected and every camera off", server, cams)
		}
	})
}

func TestSavedConnectedStartsConnected(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		body, bf, bl := linked("body")
		phone, pf := fed("phone")
		console := New(t.Context(), []Named{body, phone}, saved(t, true))
		synctest.Wait()
		bf.watch(t, 0) <- Status{Recording: true}
		pf.watch(t, 0) <- Status{}
		synctest.Wait()
		if joins, leaves := bl.counts(); joins != 1 || leaves != 0 {
			t.Errorf("link: %d joins, %d leaves; want it joined once", joins, leaves)
		}
		if server, cams := look(t, console); server != isConnected || cams != "body=recording phone=idle" {
			t.Errorf("server %+v, cameras %q; want Connected and both cameras up", server, cams)
		}
	})
}

func TestSavedConnectedWithNoCameraAnsweringStartsDisconnected(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		body, _, bl := linked("body")
		bl.errs = []error{errors.New("cam2: not joined to DIRECT-x within 15s")}
		state := saved(t, true)
		console := New(t.Context(), []Named{body}, state)
		synctest.Wait()
		if connected, _ := state.Connected(); connected {
			t.Error("saved state Connected, want Disconnected")
		}
		if _, leaves := bl.counts(); leaves != 1 {
			t.Errorf("link left %d times, want once", leaves)
		}
		if server, cams := look(t, console); server != isDisconnected || cams != "body=off!" {
			t.Errorf("server %+v, cameras %q; want Disconnected and the camera off with its error", server, cams)
		}
	})
}

func TestConnectJoinsTheLinksOneAtATimeAndWatchesEveryCamera(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r, hold := &radio{}, make(chan struct{})
		a, af, al := linked("a")
		b, bf, bl := linked("b")
		al.radio, al.hold, bl.radio, bl.hold = r, hold, r, hold
		phone, pf := fed("phone")
		state := saved(t, false)
		console := New(t.Context(), []Named{a, b, phone}, state)
		synctest.Wait()

		answer := post(console, "/connect")
		synctest.Wait()
		if n := r.inFlight.Load(); n != 1 {
			t.Errorf("%d joins in flight, want one at a time", n)
		}
		if connected, _ := state.Connected(); !connected {
			t.Error("saved state Disconnected during the Connect, want Connected")
		}
		pf.watch(t, 0) <- Status{} // watched while the links join
		close(hold)
		synctest.Wait()
		af.watch(t, 0) <- Status{}
		bf.watch(t, 0) <- Status{Recording: true}
		synctest.Wait()

		rep := connectAnswer(t, answer)
		if rep.Connection != connected || rep.Error != "" || summary(rep.Cameras) != "a=idle b=recording phone=idle" {
			t.Errorf("answer %+v, want Connected with every camera up", rep)
		}
		if n := r.most.Load(); n != 1 {
			t.Errorf("%d joins at once, want one at a time", n)
		}
		if server, _ := look(t, console); server != isConnected {
			t.Errorf("server %+v, want Connected", server)
		}
	})
}

func TestConnectGivesEachCameraOneTry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		joinErr := errors.New("wlan1: not joined to DIRECT-a within 15s")
		watchErr := errors.New("event websocket: connection refused")
		a, _, al := linked("a")
		al.errs = []error{joinErr}
		b, bf := fed("b")
		bf.failWatch(watchErr)
		c, cf := fed("c")
		console := New(t.Context(), []Named{a, b, c}, saved(t, false))
		synctest.Wait()

		answer := post(console, "/connect")
		synctest.Wait()
		cf.watch(t, 0) <- Status{}
		synctest.Wait()
		rep := connectAnswer(t, answer)
		if rep.Connection != connected || summary(rep.Cameras) != "a=off! b=off! c=idle" {
			t.Fatalf("answer %+v, want Connected with a and b off", rep)
		}
		if rep.Cameras[0].Error != joinErr.Error() || rep.Cameras[1].Error != watchErr.Error() {
			t.Errorf("errors %q and %q, want the join's and the watch's", rep.Cameras[0].Error, rep.Cameras[1].Error)
		}
		time.Sleep(time.Hour)
		synctest.Wait()
		if joins, _ := al.counts(); joins != 1 || bf.watches.Load() != 1 {
			t.Errorf("an hour later: %d joins of a, %d watches of b; want one try each", joins, bf.watches.Load())
		}
	})
}

func TestConnectWithNoCameraAnsweringLeavesTheServerDisconnected(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, af, al := linked("a")
		al.errs = []error{nil}
		b, bf := fed("b")
		bf.failWatch(errors.New("event websocket: connection refused"))
		af.failWatch(errors.New("getEvent: camera error 40401 (Camera Not Ready)"))
		state := saved(t, false)
		console := New(t.Context(), []Named{a, b}, state)
		synctest.Wait()

		rep := connect(t, console)
		if rep.Connection != disconnected || rep.Error != "no camera answered" || summary(rep.Cameras) != "a=off! b=off!" {
			t.Errorf("answer %+v, want Disconnected because no camera answered", rep)
		}
		if connected, _ := state.Connected(); connected {
			t.Error("saved state Connected, want Disconnected")
		}
		if joins, leaves := al.counts(); joins != 1 || leaves != 2 || !al.last().closed.Load() {
			t.Errorf("link: %d joins, %d leaves, watch closed %v; want it left after its join, its watch closed",
				joins, leaves, al.last().closed.Load())
		}
		if server, cams := look(t, console); server != isDisconnected || cams != "a=off! b=off!" {
			t.Errorf("server %+v, cameras %q; want Disconnected, each camera with its error", server, cams)
		}
	})
}

func TestConnectRetriesOnlyTheCamerasNotUp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, af, al := linked("a")
		b, bf, bl := linked("b")
		bl.errs = []error{errors.New("cam2: join failed")}
		console := New(t.Context(), []Named{a, b}, saved(t, false))
		synctest.Wait()

		answer := post(console, "/connect")
		synctest.Wait()
		af.watch(t, 0) <- Status{}
		synctest.Wait()
		if got := summary(connectAnswer(t, answer).Cameras); got != "a=idle b=off!" {
			t.Fatalf("first Connect: %q, want b off", got)
		}
		answer = post(console, "/connect")
		synctest.Wait()
		bf.watch(t, 0) <- Status{}
		synctest.Wait()
		if got := summary(connectAnswer(t, answer).Cameras); got != "a=idle b=idle" {
			t.Errorf("second Connect: %q, want both up", got)
		}
		aj, _ := al.counts()
		bj, _ := bl.counts()
		if aj != 1 || bj != 2 || af.watches.Load() != 1 {
			t.Errorf("joins: a %d, b %d; a watched %d times; want only b tried again", aj, bj, af.watches.Load())
		}
	})
}

func TestConnectDuringAConnectAddsNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hold := make(chan struct{})
		a, af, al := linked("a")
		al.hold = hold
		console := New(t.Context(), []Named{a}, saved(t, false))
		synctest.Wait()

		first := post(console, "/connect")
		synctest.Wait()
		second := post(console, "/connect")
		synctest.Wait()
		close(hold)
		synctest.Wait()
		af.watch(t, 0) <- Status{}
		synctest.Wait()
		for i, answer := range []<-chan *httptest.ResponseRecorder{first, second} {
			if rep := connectAnswer(t, answer); rep.Connection != connected || summary(rep.Cameras) != "a=idle" {
				t.Errorf("answer %d: %+v, want Connected with a up", i+1, rep)
			}
		}
		if joins, _ := al.counts(); joins != 1 || af.watches.Load() != 1 {
			t.Errorf("%d joins, %d watches; want one try", joins, af.watches.Load())
		}
	})
}

func TestDisconnectDuringAConnectWins(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, _, al := linked("a")
		al.hold = make(chan struct{}) // the join never completes
		b, bf := fed("b")
		state := saved(t, false)
		console := New(t.Context(), []Named{a, b}, state)
		synctest.Wait()

		answer := post(console, "/connect")
		synctest.Wait()
		bf.watch(t, 0) <- Status{Recording: true}
		synctest.Wait()
		d := disconnect(t, console)
		if d.Connection != disconnected || !slices.Equal(d.Recording, []string{"b"}) {
			t.Errorf("Disconnect's answer %+v, want Disconnected with b recording", d)
		}
		rep := connectAnswer(t, answer)
		if rep.Connection != disconnected || rep.Error != "ended by Disconnect" || summary(rep.Cameras) != "a=off b=off" {
			t.Errorf("Connect's answer %+v, want Disconnected, ended by the Disconnect", rep)
		}
		if connected, _ := state.Connected(); connected {
			t.Error("saved state Connected, want Disconnected")
		}
		if _, leaves := al.counts(); leaves != 2 || al.leftInJoin {
			t.Errorf("link left %d times, during its join %v; want left once more, after the join ended", leaves, al.leftInJoin)
		}
		if bf.watching() {
			t.Error("b still watched")
		}
		if server, cams := look(t, console); server != isDisconnected || cams != "a=off b=off" {
			t.Errorf("server %+v, cameras %q; want Disconnected", server, cams)
		}
	})
}

func TestDisconnectSavesFirstThenEndsTheWatchesAndLeaves(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, af, al := linked("a")
		b, bf := fed("b")
		c, cf := fed("c")
		cf.failWatch(errors.New("event websocket: connection refused"))
		state := saved(t, true)
		var savedAtLeave, watchedAtLeave bool
		al.onLeave = func() {
			savedAtLeave, _ = state.Connected()
			watchedAtLeave = af.watching() || bf.watching()
		}
		console := New(t.Context(), []Named{a, b, c}, state)
		synctest.Wait()
		af.watch(t, 0) <- Status{Recording: true}
		bf.watch(t, 0) <- Status{}
		synctest.Wait()

		d := disconnect(t, console)
		if d.Connection != disconnected || !slices.Equal(d.Recording, []string{"a"}) {
			t.Errorf("answer %+v, want Disconnected with a recording", d)
		}
		if _, leaves := al.counts(); leaves != 1 || savedAtLeave || watchedAtLeave {
			t.Errorf("link left %d times; at the leave the saved state was Connected %v, a camera watched %v; want left once, after the save and the watches",
				leaves, savedAtLeave, watchedAtLeave)
		}
		if !al.last().closed.Load() {
			t.Error("the link's watch is still open")
		}
		if server, cams := look(t, console); server != isDisconnected || cams != "a=off b=off c=off" {
			t.Errorf("server %+v, cameras %q; want Disconnected, every camera off", server, cams)
		}
		if a.Camera.(*scripted).stops.Load() != 0 {
			t.Error("Disconnect stopped a camera's recording")
		}
	})
}

func TestADroppedLinkGetsOneTryToComeBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		joinErr := errors.New("wlan1: not joined to DIRECT-a within 15s")
		a, af, al := linked("a")
		al.errs = []error{nil, joinErr}
		b, bf := fed("b")
		console := New(t.Context(), []Named{a, b}, saved(t, true))
		synctest.Wait()
		af.watch(t, 0) <- Status{}
		bf.watch(t, 0) <- Status{}
		synctest.Wait()

		al.last().lose(errors.New("wlan1: lost DIRECT-a: CTRL-EVENT-DISCONNECTED"))
		synctest.Wait()
		if server, cams := look(t, console); server != isConnected || cams != "a=off! b=idle" {
			t.Errorf("server %+v, cameras %q; want Connected with a off", server, cams)
		}
		if af.watching() {
			t.Error("a still watched after its link was lost")
		}
		time.Sleep(time.Hour)
		synctest.Wait()
		if joins, _ := al.counts(); joins != 2 {
			t.Errorf("%d joins an hour later, want the first and one more try", joins)
		}

		// The next Connect tries it again; a loss the try recovers from
		// leaves it up.
		answer := post(console, "/connect")
		synctest.Wait()
		af.watch(t, 0) <- Status{}
		synctest.Wait()
		if got := summary(connectAnswer(t, answer).Cameras); got != "a=idle b=idle" {
			t.Errorf("Connect: %q, want a up again", got)
		}
		al.last().lose(errors.New("wlan1: lost DIRECT-a: CTRL-EVENT-DISCONNECTED"))
		synctest.Wait()
		af.watch(t, 0) <- Status{Recording: true}
		synctest.Wait()
		if _, cams := look(t, console); cams != "a=recording b=idle" {
			t.Errorf("cameras %q, want a back up", cams)
		}
		if joins, _ := al.counts(); joins != 4 {
			t.Errorf("%d joins, want 4", joins)
		}
	})
}

func TestAWatchThatEndsGetsOneTryToComeBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b, bf := fed("b")
		console := New(t.Context(), []Named{b}, saved(t, true))
		synctest.Wait()
		ch := bf.watch(t, 0)
		ch <- Status{Recording: true}
		synctest.Wait()

		close(ch)
		synctest.Wait()
		ch = bf.watch(t, 0)
		ch <- Status{Recording: true}
		synctest.Wait()
		if _, cams := look(t, console); cams != "b=recording" {
			t.Errorf("cameras %q, want b back up", cams)
		}

		bf.failWatch(errors.New("event websocket: connection refused"))
		close(ch)
		synctest.Wait()
		time.Sleep(time.Hour)
		synctest.Wait()
		if server, cams := look(t, console); server != isConnected || cams != "b=off!" {
			t.Errorf("server %+v, cameras %q; want Connected with b off", server, cams)
		}
		if n := bf.watches.Load(); n != 3 {
			t.Errorf("%d watches, want 3: one try after each end", n)
		}
	})
}

func TestCommandsWhileDisconnectedCommandNoCamera(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, af := fed("a")
		console := New(t.Context(), []Named{a}, saved(t, false))
		synctest.Wait()
		for _, target := range []string{"/start", "/stop", "/a/start", "/a/stop"} {
			states := ask(t, console, http.MethodPost, target)
			if got := summary(states); got != "a=off!" || states[0].Error != "not connected" {
				t.Errorf("%s: %q with error %q, want a not connected", target, got, states[0].Error)
			}
		}
		if af.starts.Load()+af.stops.Load() != 0 {
			t.Error("a camera not connected was commanded")
		}
	})
}

func TestThePictureNeedsTheCameraConnectedAndEndsWithIt(t *testing.T) {
	fake := sonytest.NewCamera(t)
	cam, f := relayed(t, "cam", fake.Endpoint())
	console := New(t.Context(), []Named{cam}, saved(t, false))
	srv := httptest.NewServer(console)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/cam/liveview")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || len(fake.Calls()) != 0 {
		t.Fatalf("picture while disconnected: %s, camera calls %v; want 503 and none", resp.Status, fake.Calls())
	}

	answer := post(console, "/connect")
	upPlayable(t, f)
	if rep := connectAnswer(t, answer); rep.Connection != connected {
		t.Fatalf("Connect: %+v", rep)
	}
	awaitPicture(t, console, "cam")
	if got, want := fake.Calls(), []string{"startLiveview@1.0"}; !slices.Equal(got, want) {
		t.Fatalf("camera calls once connected, before any viewer, %v; want %v", got, want)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	resp, _ = openStream(t, ctx, srv.URL+"/cam/liveview")
	if _, err := resp.Body.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	disconnect(t, console)
	ended := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, resp.Body)
		ended <- err
	}()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the picture still streams 5 s after Disconnect")
	}
	srv.Close()
	if got, want := fake.Calls(), []string{"startLiveview@1.0", "stopLiveview@1.0"}; !slices.Equal(got, want) {
		t.Errorf("camera calls %v, want %v", got, want)
	}
}

// stream opens the console's event stream in the test's bubble and returns
// its events as they arrive, as events gives them.
func stream(t *testing.T, console http.Handler) <-chan string {
	t.Helper()
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		console.ServeHTTP(&pipeWriter{header: http.Header{}, body: pw}, httptest.NewRequestWithContext(ctx, http.MethodGet, "/events", nil))
		pw.Close()
	}()
	events := make(chan string, 64)
	go readEvents(bufio.NewScanner(pr), events)
	t.Cleanup(func() {
		cancel()
		pr.Close()
		<-done
	})
	return events
}

// waiting is the next event of events, failing unless one is waiting.
func waiting(t *testing.T, events <-chan string) string {
	t.Helper()
	select {
	case e := <-events:
		return e
	default:
		t.Fatal("no event waiting")
		return ""
	}
}

// awaited is a camera's precondition, which holds once the test closes
// holds; waits counts its Waits.
type awaited struct {
	name  string
	holds chan struct{}
	waits atomic.Int32
}

func newAwaited(name string) *awaited { return &awaited{name: name, holds: make(chan struct{})} }

func (p *awaited) Wait(ctx context.Context) error {
	p.waits.Add(1)
	select {
	case <-p.holds:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *awaited) String() string { return p.name }

func TestATryWaitsForItsCamerasPrecondition(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		body, bf, bl := linked("body")
		wifi := newAwaited("body wifi")
		body.Precondition = wifi
		phone, pf := fed("phone")
		network := newAwaited("network")
		phone.Precondition = network
		state := saved(t, true)
		console := New(t.Context(), []Named{body, phone}, state)
		time.Sleep(time.Hour)
		synctest.Wait()
		if joins, _ := bl.counts(); joins != 0 || bf.watches.Load()+pf.watches.Load() != 0 {
			t.Errorf("an hour without the preconditions: %d joins, %d watches; want no try", joins, bf.watches.Load()+pf.watches.Load())
		}
		if connected, _ := state.Connected(); !connected {
			t.Error("saved state Disconnected while the cameras wait, want Connected")
		}
		if server, cams := look(t, console); server.Connection != connected || cams != "body=connecting phone=connecting" {
			t.Errorf("server %+v, cameras %q; want Connected with both cameras waiting", server, cams)
		}

		close(wifi.holds)
		synctest.Wait()
		if joins, _ := bl.counts(); joins != 1 {
			t.Errorf("%d joins once the body's precondition holds, want its try", joins)
		}
		bf.watch(t, 0) <- Status{}
		synctest.Wait()
		if pf.watches.Load() != 0 {
			t.Error("phone tried before its precondition holds")
		}
		close(network.holds)
		synctest.Wait()
		pf.watch(t, 0) <- Status{Recording: true}
		synctest.Wait()
		if server, cams := look(t, console); server != isConnected || cams != "body=idle phone=recording" {
			t.Errorf("server %+v, cameras %q; want Connected with both cameras up", server, cams)
		}
		if connected, _ := state.Connected(); !connected {
			t.Error("saved state Disconnected, want Connected")
		}
	})
}

func TestDisconnectDuringAWaitMakesNoTry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		body, bf, bl := linked("body")
		wifi := newAwaited("body wifi")
		body.Precondition = wifi
		console := New(t.Context(), []Named{body}, saved(t, false))
		synctest.Wait()

		answer := post(console, "/connect")
		synctest.Wait()
		if wifi.waits.Load() != 1 {
			t.Fatalf("%d waits for the precondition, want the try waiting", wifi.waits.Load())
		}
		disconnect(t, console)
		if rep := connectAnswer(t, answer); rep.Connection != disconnected || rep.Error != "ended by Disconnect" {
			t.Errorf("Connect's answer %+v, want Disconnected, ended by the Disconnect", rep)
		}
		close(wifi.holds)
		time.Sleep(time.Hour)
		synctest.Wait()
		if joins, _ := bl.counts(); joins != 0 || bf.watches.Load() != 0 {
			t.Errorf("%d joins, %d watches; want no try", joins, bf.watches.Load())
		}
		if server, cams := look(t, console); server != isDisconnected || cams != "body=off" {
			t.Errorf("server %+v, cameras %q; want Disconnected with the body off and no error", server, cams)
		}
	})
}

func TestACameraWaitingHoldsBackNoOther(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, af, al := linked("a")
		a.Precondition = newAwaited("a wifi") // never holds
		b, bf, bl := linked("b")
		phone, pf := fed("phone")
		console := New(t.Context(), []Named{a, b, phone}, saved(t, true))
		synctest.Wait()
		bf.watch(t, 0) <- Status{}
		pf.watch(t, 0) <- Status{Recording: true}
		synctest.Wait()
		if server, cams := look(t, console); server.Connection != connected || cams != "a=connecting b=idle phone=recording" {
			t.Errorf("server %+v, cameras %q; want Connected with b and phone up while a waits", server, cams)
		}
		aj, _ := al.counts()
		bj, _ := bl.counts()
		if aj != 0 || bj != 1 || af.watches.Load() != 0 {
			t.Errorf("joins: a %d, b %d; a watched %d times; want only b joined", aj, bj, af.watches.Load())
		}
	})
}

func TestAWaitingCameraShowsWhatItWaitsFor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		body, _, _ := linked("rx10m4")
		wifi := newAwaited("rx10m4 wifi")
		body.Precondition = wifi
		phone, _ := fed("pixel9")
		phone.Precondition = newAwaited("network")
		console := New(t.Context(), []Named{body, phone}, saved(t, true))
		synctest.Wait()
		events := stream(t, console)
		synctest.Wait()
		waiting(t, events) // the server's connection
		for _, want := range []state{
			{Name: "rx10m4", Connection: connecting, Waiting: "rx10m4 wifi"},
			{Name: "pixel9", Connection: connecting, Waiting: "network"},
		} {
			var s state
			if err := json.Unmarshal([]byte(waiting(t, events)), &s); err != nil {
				t.Fatal(err)
			}
			if !s.same(want) {
				t.Errorf("state %+v, want %+v", s, want)
			}
		}

		close(wifi.holds)
		synctest.Wait()
		var s state
		if err := json.Unmarshal([]byte(waiting(t, events)), &s); err != nil {
			t.Fatal(err)
		}
		if want := (state{Name: "rx10m4", Connection: connecting}); !s.same(want) {
			t.Errorf("once its precondition holds: %+v, want %+v", s, want)
		}
	})
}

// The page shows a camera waiting as what it waits for.
var waitingText = regexp.MustCompile(`s\.waiting \? 'waiting for ' \+ s\.waiting :`)

func TestScriptShowsWhatATileWaitsFor(t *testing.T) {
	cam, _ := fed("pixel9")
	m := script.FindStringSubmatch(get(New(t.Context(), []Named{cam}, saved(t, false)), "/").Body.String())
	if m == nil {
		t.Fatal("page has no script")
	}
	if !waitingText.MatchString(m[1]) {
		t.Errorf("script does not show what a tile waits for: %q", m[1])
	}
}
