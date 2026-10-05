package rig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/cehbz/multicam/internal/blackmagic"
	"github.com/cehbz/multicam/internal/console"
	"github.com/cehbz/multicam/internal/link"
	"github.com/cehbz/multicam/internal/mediamtx"
	"github.com/cehbz/multicam/internal/sony"
	"github.com/cehbz/multicam/internal/sony/sonytest"
)

// onLink is a Sony body joined on iface to network with password, its
// route in table.
func onLink(iface, network, password string, table int) sonyBody {
	return sonyBody{Interface: iface, Endpoint: sony.DefaultEndpoint, Network: network, Password: password, Table: table}
}

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		want    []camera
		wantErr string // part of the error; empty when the config is valid
	}{
		{
			name: "two Sony bodies on their links, each with its own table",
			text: "[[camera]]\nname = \"rx10m4\"\nkind = \"sony\"\ninterface = \"wlan1\"\nnetwork = \"DIRECT-a\"\npassword = \"pa\"\n" +
				"[[camera]]\nname = \"pixel9\"\nkind = \"blackmagic\"\nid = \"b722\"\ninterface = \"wlan0\"\n" +
				"[[camera]]\nname = \"rx100m6\"\nkind = \"sony\"\ninterface = \"cam2\"\nnetwork = \"DIRECT-b\"\npassword = \"pb\"\nstart_gap = \"3s\"\n",
			want: []camera{
				{"rx10m4", onLink("wlan1", "DIRECT-a", "pa", 2001)},
				{"pixel9", blackmagicPhone{ID: "b722", Interface: "wlan0"}},
				{"rx100m6", func() sonyBody { b := onLink("cam2", "DIRECT-b", "pb", 2002); b.StartGap = 3 * time.Second; return b }()},
			},
		},
		{
			name: "Sony body with an endpoint and no interface",
			text: "[[camera]]\nname = \"fake-1\"\nkind = \"sony\"\nendpoint = \"http://127.0.0.1:9/sony/camera\"\n",
			want: []camera{{"fake-1", sonyBody{Endpoint: "http://127.0.0.1:9/sony/camera"}}},
		},
		{name: "interface without a network", text: "[[camera]]\nname = \"a\"\nkind = \"sony\"\ninterface = \"wlan1\"\npassword = \"p\"\n", wantErr: "camera 1 (a): a body on an interface needs its network and password"},
		{name: "interface without a password", text: "[[camera]]\nname = \"a\"\nkind = \"sony\"\ninterface = \"wlan1\"\nnetwork = \"n\"\n", wantErr: "camera 1 (a): a body on an interface needs its network and password"},
		{name: "network without an interface", text: "[[camera]]\nname = \"a\"\nkind = \"sony\"\nnetwork = \"n\"\npassword = \"p\"\n", wantErr: "camera 1 (a): network and password need an interface"},
		{
			name:    "two bodies on one interface",
			text:    "[[camera]]\nname = \"a\"\nkind = \"sony\"\ninterface = \"wlan1\"\nnetwork = \"n\"\npassword = \"p\"\n[[camera]]\nname = \"b\"\nkind = \"sony\"\ninterface = \"wlan1\"\nnetwork = \"m\"\npassword = \"p\"\n",
			wantErr: `camera 2 (b): interface "wlan1" is already camera 1's`,
		},
		{
			name: "Blackmagic camera by its id on an interface",
			text: "[[camera]]\nname = \"pixel9\"\nkind = \"blackmagic\"\nid = \"b722\"\ninterface = \"wlan0\"\n",
			want: []camera{{"pixel9", blackmagicPhone{ID: "b722", Interface: "wlan0"}}},
		},
		{name: "Blackmagic camera without an id", text: "[[camera]]\nname = \"a\"\nkind = \"blackmagic\"\ninterface = \"wlan0\"\n", wantErr: "camera 1 (a): a Blackmagic camera needs its id and interface"},
		{name: "Blackmagic camera without an interface", text: "[[camera]]\nname = \"a\"\nkind = \"blackmagic\"\nid = \"b722\"\n", wantErr: "camera 1 (a): a Blackmagic camera needs its id and interface"},
		{name: "a Blackmagic camera's address is gone", text: "[[camera]]\nname = \"a\"\nkind = \"blackmagic\"\nid = \"b722\"\ninterface = \"wlan0\"\naddress = \"192.168.1.9:4444\"\n", wantErr: `camera 1 (a): unknown key "address"`},
		{name: "start gap that is not a duration", text: "[[camera]]\nname = \"a\"\nkind = \"sony\"\nstart_gap = \"soon\"\n", wantErr: `camera 1 (a): start_gap "soon" is not a duration`},
		{name: "no cameras", text: "", wantErr: "no cameras"},
		{name: "camera without a name", text: "[[camera]]\nkind = \"sony\"\n", wantErr: "camera 1"},
		{name: "empty name", text: "[[camera]]\nname = \"\"\nkind = \"sony\"\n", wantErr: "camera 1"},
		{name: "name with a slash", text: "[[camera]]\nname = \"rx/10\"\nkind = \"sony\"\n", wantErr: `"rx/10"`},
		{name: "name with a space", text: "[[camera]]\nname = \"rx 10\"\nkind = \"sony\"\n", wantErr: `"rx 10"`},
		{name: "name of dots", text: "[[camera]]\nname = \"..\"\nkind = \"sony\"\n", wantErr: `".."`},
		{
			name:    "duplicate name",
			text:    "[[camera]]\nname = \"a\"\nkind = \"sony\"\n[[camera]]\nname = \"b\"\nkind = \"sony\"\n[[camera]]\nname = \"a\"\nkind = \"sony\"\n",
			wantErr: `camera 3: name "a" is already camera 1`,
		},
		{
			name:    "no kind",
			text:    "[[camera]]\nname = \"rx10m4\"\ninterface = \"wlan1\"\n",
			wantErr: `camera 1 (rx10m4): no kind: add kind = "sony" or kind = "blackmagic"`,
		},
		{name: "unknown kind", text: "[[camera]]\nname = \"a\"\nkind = \"gopro\"\n", wantErr: `camera 1 (a): kind "gopro" is not "sony" or "blackmagic"`},
		{name: "the Pixel kind is gone", text: "[[camera]]\nname = \"a\"\nkind = \"pixel\"\naddress = \"1.2.3.4:5\"\n", wantErr: `camera 1 (a): kind "pixel" is not "sony" or "blackmagic"`},
		{
			name:    "misspelled key",
			text:    "[[camera]]\nname = \"a\"\nkind = \"sony\"\ninteface = \"wlan1\"\n",
			wantErr: `camera 1 (a): unknown key "inteface"`,
		},
		{name: "key that is not a string", text: "[[camera]]\nname = \"a\"\nkind = \"sony\"\ninterface = 1\n", wantErr: "camera 1 (a): interface"},
		{name: "unknown top-level key", text: "abd = 1\n[[camera]]\nname = \"a\"\nkind = \"sony\"\n", wantErr: "unknown key abd"},
		{name: "the adb table is gone", text: "[adb]\npath = \"/x\"\n[[camera]]\nname = \"a\"\nkind = \"sony\"\n", wantErr: "unknown key adb"},
		{name: "not TOML", text: "[[camera]\n", wantErr: "toml: line"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parse(tt.text)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("parse = %v, %v; want an error containing %q", got.cameras, err, tt.wantErr)
				}
				return
			}
			if err != nil || !slices.Equal(got.cameras, tt.want) {
				t.Errorf("parse = %v, %v; want %v", got.cameras, err, tt.want)
			}
		})
	}
}

const (
	exampleConfig = "../../multicam.example.toml"
	phoneConfig   = "../../multicam.phone.toml"
)

// exampleCameras are the example config's cameras.
var exampleCameras = []camera{
	{"rx10m4", onLink("wlan1", "DIRECT-xxxx:DSC-RX10M4", "password", 2001)},
	{"rx100m6", func() sonyBody {
		b := onLink("cam2", "DIRECT-xxxx:DSC-RX100M6", "password", 2002)
		b.StartGap = 3 * time.Second
		return b
	}()},
	{"pixel9", blackmagicPhone{ID: "b722b4654dc94e5dbb76a30055bf6a72", Interface: "wlan0"}},
}

func TestExampleConfig(t *testing.T) {
	text, err := os.ReadFile(exampleConfig)
	if err != nil {
		t.Fatal(err)
	}
	got, err := parse(string(text))
	if err != nil || !slices.Equal(got.cameras, exampleCameras) {
		t.Errorf("parse = %v, %v; want %v", got.cameras, err, exampleCameras)
	}
	want := &mediamtx.Server{
		Path: "/data/local/tmp/mc/mtx/mediamtx", Dir: "/data/local/tmp/mc/mtx",
		Log: "/data/local/tmp/mc/mtx/mediamtx.log", RestartDelay: time.Second,
	}
	if !reflect.DeepEqual(got.mediaMTX, want) {
		t.Errorf("MediaMTX %+v, want %+v", got.mediaMTX, want)
	}
	if got.links != (link.Options{}) || got.state != "" {
		t.Errorf("links %+v, state %q; want the defaults", got.links, got.state)
	}
}

// The phone's config, which is not checked in, is the example with the
// bodies' credentials.
func TestPhoneConfigIsTheExampleWithCredentials(t *testing.T) {
	text, err := os.ReadFile(phoneConfig)
	if errors.Is(err, os.ErrNotExist) {
		t.Skip("no phone config")
	}
	if err != nil {
		t.Fatal(err)
	}
	got, err := parse(string(text))
	if err != nil {
		t.Fatal(err)
	}
	var cameras []camera
	for _, c := range got.cameras {
		if b, ok := c.Kind.(sonyBody); ok {
			if b.Network == "" || strings.Contains(b.Network, "xxxx") || b.Password == "" || b.Password == "password" {
				t.Errorf("camera %s: placeholder credentials", c.Name)
			}
			b.Network, b.Password = "", ""
			c.Kind = b
		}
		cameras = append(cameras, c)
	}
	var want []camera
	for _, c := range exampleCameras {
		if b, ok := c.Kind.(sonyBody); ok {
			b.Network, b.Password = "", ""
			c.Kind = b
		}
		want = append(want, c)
	}
	if !slices.Equal(cameras, want) {
		t.Errorf("cameras without credentials %v, want the example's %v", cameras, want)
	}
}

func TestLoadGivesEachCameraItsOwnBody(t *testing.T) {
	first, second := sonytest.NewCamera(t), sonytest.NewCamera(t)
	path := filepath.Join(t.TempDir(), "multicam.toml")
	config := fmt.Sprintf("[[camera]]\nname = \"first\"\nkind = \"sony\"\nendpoint = %q\n[[camera]]\nname = \"second\"\nkind = \"sony\"\nendpoint = %q\n",
		first.Endpoint(), second.Endpoint())
	if err := os.WriteFile(path, []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	rig, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(rig.Cameras) != 2 || rig.Cameras[0].Name != "first" || rig.Cameras[1].Name != "second" {
		t.Fatalf("rig cameras %v, want first and second", rig.Cameras)
	}
	if _, ok := rig.Cameras[0].Picture.(console.Relayed); !ok {
		t.Errorf("a Sony body's picture is %T, want one the console relays", rig.Cameras[0].Picture)
	}
	if err := rig.Cameras[1].StartRecording(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if _, err := rig.Cameras[0].Watch(ctx); err != nil {
		t.Fatal(err)
	}
	if got, want := first.Calls()[:1], []string{"getEvent@1.3"}; !slices.Equal(got, want) {
		t.Errorf("first camera's calls %v, want %v", got, want)
	}
	if got, want := second.Calls(), []string{"startMovieRec@1.0"}; !slices.Equal(got, want) {
		t.Errorf("second camera's calls %v, want %v", got, want)
	}
}

// receive is the next status ch delivers, failing when none comes in a
// second.
func receive(t *testing.T, ch <-chan console.Status) console.Status {
	t.Helper()
	select {
	case v, ok := <-ch:
		if !ok {
			t.Fatal("the watch ended")
		}
		return v
	case <-time.After(time.Second):
		t.Fatal("no status delivered")
		return console.Status{}
	}
}

func TestSonyBodyIsRecordingWhileItsStatusIsMovieRecording(t *testing.T) {
	fake := sonytest.NewCamera(t)
	fake.ListLiveview(true)
	cam, err := sonyBody{Endpoint: fake.Endpoint()}.open("body", noKeeper)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ch, err := cam.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := receive(t, ch), (console.Status{Picture: true}); got != want {
		t.Errorf("IDLE body's status %+v, want %+v", got, want)
	}
	for _, c := range []struct {
		status    string
		recording bool
	}{{"MovieWaitRecStart", false}, {"MovieRecording", true}, {"MovieWaitRecStop", false}, {"MovieSaving", false}, {"IDLE", false}} {
		fake.Push(c.status)
		if got, want := receive(t, ch), (console.Status{Recording: c.recording, Picture: true}); got != want {
			t.Errorf("status on %s %+v, want %+v", c.status, got, want)
		}
	}
	cancel()
	if _, ok := <-ch; ok {
		t.Error("the watch delivered after its context ended")
	}
}

// A Sony body's picture, which the console relays, can be played while the
// body lists startLiveview.
func TestSonyBodysPictureCanBePlayedWhileItListsStartLiveview(t *testing.T) {
	fake := sonytest.NewCamera(t)
	cam, err := sonyBody{Endpoint: fake.Endpoint()}.open("body", noKeeper)
	if err != nil {
		t.Fatal(err)
	}
	ch, err := cam.Watch(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := receive(t, ch), (console.Status{}); got != want {
		t.Errorf("status before startLiveview is listed %+v, want %+v", got, want)
	}
	apis := func(names ...string) map[string]any {
		return map[string]any{"type": "availableApiList", "names": names}
	}
	for _, step := range []struct {
		result []any
		want   console.Status
	}{
		{[]any{apis("getEvent", "startLiveview", "stopLiveview")}, console.Status{Picture: true}},
		{[]any{nil, map[string]any{"type": "cameraStatus", "cameraStatus": "MovieRecording"}}, console.Status{Recording: true, Picture: true}},
		{[]any{apis("getEvent", "stopMovieRec")}, console.Status{Recording: true}},
	} {
		fake.PushResult(step.result...)
		if got := receive(t, ch); got != step.want {
			t.Errorf("after %v: status %+v, want %+v", step.result, got, step.want)
		}
	}
}

func TestSonyBodyWatchFailureIsReturned(t *testing.T) {
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close() // its address now refuses connections
	cam, err := sonyBody{Endpoint: gone.URL + "/sony/camera"}.open("body", noKeeper)
	if err != nil {
		t.Fatal(err)
	}
	if ch, err := cam.Watch(t.Context()); err == nil || !strings.HasPrefix(err.Error(), "getEvent: ") {
		t.Errorf("Watch = %v, %v; want no channel and the failed status read", ch, err)
	}
}

func TestBlackmagicPhoneIsStreamedUnderItsName(t *testing.T) {
	cam, err := blackmagicPhone{ID: "b722", Interface: "wlan0"}.open("pixel9", noKeeper)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cam.Picture, (console.Streamed{Path: "pixel9"}); got != want {
		t.Errorf("picture %#v, want %#v", got, want)
	}
	if cam.Name != "pixel9" || cam.Camera == nil || cam.Link != nil {
		t.Errorf("camera %#v, want pixel9 with a camera and no link", cam)
	}
	if bc, ok := cam.Camera.(*blackmagicCamera); !ok || bc.path != "pixel9" || bc.source == nil {
		t.Errorf("camera %#v, want its livestream published under pixel9 from a route's source", cam.Camera)
	}
}

func TestBlackmagicPhoneWaitsToBeFound(t *testing.T) {
	cam, err := blackmagicPhone{ID: "b722", Interface: "wlan0"}.open("pixel9", noKeeper)
	if err != nil {
		t.Fatal(err)
	}
	if cam.Precondition == nil || any(cam.Precondition) != any(cam.Camera) {
		t.Fatalf("precondition %#v, want the camera found", cam.Precondition)
	}
	if got := cam.Precondition.String(); got != "Blackmagic Camera" {
		t.Errorf("String = %q, want Blackmagic Camera", got)
	}
}

// foundAt is a find that finds the app at addr.
func foundAt(t *testing.T, addr string) func(context.Context) (netip.AddrPort, error) {
	ap := netip.MustParseAddrPort(addr)
	return func(context.Context) (netip.AddrPort, error) { return ap, nil }
}

func TestBlackmagicCameraIsNotReachedBeforeItIsFound(t *testing.T) {
	cam := &blackmagicCamera{find: foundAt(t, phoneApp(t).addr)}
	if err := cam.StartRecording(t.Context()); err == nil || err.Error() != "Blackmagic Camera not found" {
		t.Errorf("StartRecording = %v, want not found", err)
	}
	if ch, err := cam.Watch(t.Context()); err == nil {
		t.Errorf("Watch = %v, %v; want not found", ch, err)
	}
}

func TestBlackmagicCameraIsReachedWhereItWasFound(t *testing.T) {
	cam := &blackmagicCamera{find: foundAt(t, phoneApp(t).addr)}
	if err := cam.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := cam.StartRecording(t.Context()); err != nil {
		t.Errorf("StartRecording = %v", err)
	}
	if err := cam.StopRecording(t.Context()); err != nil {
		t.Errorf("StopRecording = %v", err)
	}
}

func TestBlackmagicCameraWaitEndsWithTheFindsError(t *testing.T) {
	want := errors.New("listen udp4 0.0.0.0:5353: bind: permission denied")
	cam := &blackmagicCamera{find: func(context.Context) (netip.AddrPort, error) { return netip.AddrPort{}, want }}
	if err := cam.Wait(t.Context()); err != want {
		t.Errorf("Wait = %v, want %v", err, want)
	}
}

// app is Blackmagic Camera's HTTP server as a try uses it: idle, its
// livestream's active platform at staleURL, and pushing each of statuses as
// its livestream's status once subscribed.
type app struct {
	addr string // host:port

	mu  sync.Mutex
	url string // the active platform's
}

// staleURL is where an app's livestream points before a try points it.
const staleURL = "srt://192.168.1.116:8890?streamid=publish:pixel9&pkt_size=1316"

// URL is the active platform's URL.
func (a *app) URL() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.url
}

func phoneApp(t *testing.T, statuses ...string) *app {
	t.Helper()
	a := &app{url: staleURL}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + strings.TrimPrefix(r.URL.Path, blackmagic.BasePath) {
		case "GET " + blackmagic.RecordPath:
			fmt.Fprint(w, `{"recording": false}`)
		case "GET " + blackmagic.LivestreamPath:
			fmt.Fprint(w, `{"status": "Idle"}`)
		case "GET " + blackmagic.CustomPlatformsPath:
			fmt.Fprint(w, `[]`)
		case "GET " + blackmagic.ActivePlatformPath:
			json.NewEncoder(w).Encode(map[string]string{"platform": "Blackmagic Cam App SRT", "server": "Custom", "url": a.URL()})
		case "PUT " + blackmagic.ActivePlatformPath:
			var active struct{ URL string }
			json.NewDecoder(r.Body).Decode(&active)
			a.mu.Lock()
			a.url = active.URL
			a.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case "GET " + blackmagic.EventPath:
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer conn.CloseNow()
			if _, _, err := conn.Read(r.Context()); err != nil {
				return
			}
			for _, s := range statuses {
				wsjson.Write(r.Context(), conn, map[string]any{"type": "event", "data": map[string]any{
					"action": "propertyValueChanged", "property": blackmagic.LivestreamPath, "value": map[string]any{"status": s}}})
			}
			conn.Read(r.Context())
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(srv.Close)
	a.addr = strings.TrimPrefix(srv.URL, "https://")
	return a
}

// thisPhone is a source that has this phone at 192.168.1.109 on every route.
func thisPhone(netip.Addr) (netip.Addr, error) { return netip.MustParseAddr("192.168.1.109"), nil }

func TestBlackmagicCameraPointsItsLivestreamAtThisPhone(t *testing.T) {
	a := phoneApp(t)
	var dst netip.Addr
	source := func(d netip.Addr) (netip.Addr, error) { dst = d; return thisPhone(d) }
	cam := &blackmagicCamera{find: foundAt(t, a.addr), source: source, path: "pixel9"}
	if err := cam.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := cam.Watch(t.Context()); err != nil {
		t.Fatal(err)
	}
	if want := netip.MustParseAddr("127.0.0.1"); dst != want {
		t.Errorf("source of the route to %v, want to %v", dst, want)
	}
	if got, want := a.URL(), "srt://192.168.1.109:8890?streamid=publish:pixel9&pkt_size=1316"; got != want {
		t.Errorf("livestream URL %q, want %q", got, want)
	}
}

func TestBlackmagicCameraWatchFailsWithoutARouteToIt(t *testing.T) {
	want := errors.New("route to 127.0.0.1: network is unreachable")
	noRoute := func(netip.Addr) (netip.Addr, error) { return netip.Addr{}, want }
	cam := &blackmagicCamera{find: foundAt(t, phoneApp(t).addr), source: noRoute, path: "pixel9"}
	if err := cam.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	if ch, err := cam.Watch(t.Context()); !errors.Is(err, want) {
		t.Errorf("Watch = %v, %v; want %v", ch, err, want)
	}
}

func TestBlackmagicPhonesPictureCanBePlayedWhileItStreams(t *testing.T) {
	cam := &blackmagicCamera{find: foundAt(t, phoneApp(t, "Connecting", "Streaming", "Idle").addr), source: thisPhone, path: "pixel9"}
	if err := cam.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ch, err := cam.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []console.Status{{}, {Picture: true}, {}} {
		if got := receive(t, ch); got != want {
			t.Errorf("status %d %+v, want %+v", i, got, want)
		}
	}
}

func TestLoadErrorsNameTheFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "multicam.toml")
	if rig, err := Load(missing); err == nil || !strings.Contains(err.Error(), missing) {
		t.Errorf("Load of a missing file = %v, %v; want an error naming it", rig, err)
	}
	invalid := filepath.Join(t.TempDir(), "empty.toml")
	if err := os.WriteFile(invalid, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if rig, err := Load(invalid); err == nil || !strings.Contains(err.Error(), invalid) || !strings.Contains(err.Error(), "no cameras") {
		t.Errorf("Load of an empty file = %v, %v; want an error naming it and the fault", rig, err)
	}
}

func TestLoadOfInterfacesNeedsLinux(t *testing.T) {
	if runtime.GOOS == "linux" || runtime.GOOS == "android" {
		t.Skip("this system binds connections to interfaces")
	}
	rig, err := Load(exampleConfig)
	if err == nil || !strings.Contains(err.Error(), "rx10m4") || !strings.Contains(err.Error(), "wlan1") {
		t.Errorf("Load = %v, %v; want an error naming camera rx10m4 and interface wlan1", rig, err)
	}
}

func TestParseTables(t *testing.T) {
	cam := "[[camera]]\nname = \"a\"\nkind = \"sony\"\n"
	tests := []struct {
		name     string
		text     string
		mediaMTX *mediamtx.Server
		links    link.Options
		state    string
		wantErr  string
	}{
		{name: "none: no MediaMTX and the defaults", text: cam},
		{
			name:     "MediaMTX's path alone: its directory and a log in it",
			text:     "[mediamtx]\npath = \"/m/mtx/mediamtx\"\n" + cam,
			mediaMTX: &mediamtx.Server{Path: "/m/mtx/mediamtx", Dir: "/m/mtx", Log: "/m/mtx/mediamtx.log", RestartDelay: time.Second},
		},
		{
			name:     "MediaMTX's directory and log",
			text:     "[mediamtx]\npath = \"/bin/mediamtx\"\ndir = \"/etc/m\"\nlog = \"/var/m.log\"\n" + cam,
			mediaMTX: &mediamtx.Server{Path: "/bin/mediamtx", Dir: "/etc/m", Log: "/var/m.log", RestartDelay: time.Second},
		},
		{name: "MediaMTX without a path", text: "[mediamtx]\ndir = \"/x\"\n" + cam, wantErr: "mediamtx: no path"},
		{name: "MediaMTX's unknown key", text: "[mediamtx]\npath = \"/x\"\nport = 1\n" + cam, wantErr: "port"},
		{name: "MediaMTX's path not a string", text: "[mediamtx]\npath = 3\n" + cam, wantErr: "mediamtx"},
		{
			name:  "the links and the state file",
			text:  "state = \"/s/connection\"\n[link]\nsupplicant = \"/w/wpa\"\nlib = \"/w/lib\"\ndir = \"/l\"\nphy = \"phy1\"\n" + cam,
			links: link.Options{Supplicant: "/w/wpa", Lib: "/w/lib", Dir: "/l", Phy: "phy1"},
			state: "/s/connection",
		},
		{name: "the links' unknown key", text: "[link]\nsocket = \"/x\"\n" + cam, wantErr: "unknown key link.socket"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parse(tt.text)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("parse = %+v, %v; want an error with %q", got, err, tt.wantErr)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got.mediaMTX, tt.mediaMTX) || got.links != tt.links || got.state != tt.state {
				t.Errorf("parse = MediaMTX %+v, links %+v, state %q, %v; want %+v, %+v, %q",
					got.mediaMTX, got.links, got.state, err, tt.mediaMTX, tt.links, tt.state)
			}
		})
	}
}

func TestLoadKeepsTheStateBesideTheConfigByDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "multicam.toml")
	if err := os.WriteFile(path, []byte("[[camera]]\nname = \"p\"\nkind = \"blackmagic\"\nid = \"b722\"\ninterface = \"wlan0\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := Load(path)
	if err != nil || r.State != console.StateFile(filepath.Join(dir, "connection.state")) {
		t.Errorf("Load = %+v, %v; want the state file connection.state beside the config", r, err)
	}
}

// fakeKeeper records the links it is asked to join and leave, and its
// radio's waits.
type fakeKeeper struct {
	joined     []link.Config
	left       []string
	radioWaits int
	err        error
}

func (k *fakeKeeper) AwaitRadio(context.Context) error {
	k.radioWaits++
	return k.err
}

func (k *fakeKeeper) Join(_ context.Context, c link.Config) (*link.Link, error) {
	k.joined = append(k.joined, c)
	return nil, k.err
}

func (k *fakeKeeper) Leave(_ context.Context, iface string) error {
	k.left = append(k.left, iface)
	return k.err
}

func TestSonyLinkIsTheBodysLinkOnTheKeeper(t *testing.T) {
	k := &fakeKeeper{err: errors.New("cam2: not joined to DIRECT-b within 15s")}
	cfg := link.Config{Interface: "cam2", Network: "DIRECT-b", Password: "pb", Table: 2002}
	l := sonyLink{keeper: k, config: cfg}
	if j, err := l.Join(t.Context()); j != nil || err != k.err {
		t.Errorf("Join = %v, %v; want no link and the keeper's error", j, err)
	}
	if err := l.Leave(t.Context()); err != k.err {
		t.Errorf("Leave = %v, want the keeper's error", err)
	}
	if !slices.Equal(k.joined, []link.Config{cfg}) || !slices.Equal(k.left, []string{"cam2"}) {
		t.Errorf("keeper joined %v and left %v; want the body's link", k.joined, k.left)
	}
}

func TestBodyOnAnInterfaceOpensWithItsLink(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "android" {
		t.Skip("binding to an interface needs Linux")
	}
	k := &link.Keeper{}
	cam, err := onLink("cam2", "DIRECT-b", "pb", 2002).open("rx100m6", func() (*link.Keeper, error) { return k, nil })
	if err != nil {
		t.Fatal(err)
	}
	want := sonyLink{keeper: k, config: link.Config{Interface: "cam2", Network: "DIRECT-b", Password: "pb", Table: 2002}}
	if cam.Link != want {
		t.Errorf("link %#v, want %#v", cam.Link, want)
	}
	if want := (wifi{keeper: k, body: "rx100m6"}); cam.Precondition != want {
		t.Errorf("precondition %#v, want %#v", cam.Precondition, want)
	}
}

func TestWifiIsTheKeepersRadioNamedForTheBody(t *testing.T) {
	k := &fakeKeeper{err: errors.New("open /sys/class/ieee80211/phy0/index: no such file or directory")}
	w := wifi{keeper: k, body: "rx10m4"}
	if err := w.Wait(t.Context()); err != k.err || k.radioWaits != 1 {
		t.Errorf("Wait = %v after %d radio waits; want the keeper's one wait and its error", err, k.radioWaits)
	}
	if got := w.String(); got != "rx10m4 wifi" {
		t.Errorf("String = %q, want rx10m4 wifi", got)
	}
}

func TestSonyBodyWithoutAnInterfaceWaitsForNothing(t *testing.T) {
	cam, err := sonyBody{Endpoint: sony.DefaultEndpoint}.open("body", noKeeper)
	if err != nil {
		t.Fatal(err)
	}
	if cam.Precondition != nil {
		t.Errorf("precondition %#v, want none", cam.Precondition)
	}
}

func TestLoadHandsOverMediaMTX(t *testing.T) {
	path := filepath.Join(t.TempDir(), "multicam.toml")
	config := "[mediamtx]\npath = \"/x/mediamtx\"\n[[camera]]\nname = \"p\"\nkind = \"blackmagic\"\nid = \"b722\"\ninterface = \"wlan0\"\n"
	if err := os.WriteFile(path, []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := Load(path)
	if err != nil || r.MediaMTX == nil || r.MediaMTX.Path != "/x/mediamtx" {
		t.Errorf("Load = %+v, %v; want a rig with MediaMTX at /x/mediamtx", r, err)
	}
}

// noKeeper is the keeper of a rig with no links.
func noKeeper() (*link.Keeper, error) { return nil, errors.New("no links") }
