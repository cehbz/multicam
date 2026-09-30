package sony

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// Envelope shapes follow Sony's sample client (SimpleRemoteApi.java: params
// always an array, "version" a string), kota65535/sony_camera_remote_api
// (getMethodTypes answers under "results", errors as [code, message]), and the
// error-code table shared by fabnavi/SonyCameraController and
// Nailik/sony_alpha_control.

func TestRequestJSON(t *testing.T) {
	tests := []struct {
		req  Request
		want string
	}{
		{NewRequest(1, "getAvailableApiList", "1.0"), `{"method":"getAvailableApiList","params":[],"id":1,"version":"1.0"}`},
		{NewRequest(7, "getEvent", "1.0", false), `{"method":"getEvent","params":[false],"id":7,"version":"1.0"}`},
		{NewRequest(2, "actZoom", "1.0", "in", "start"), `{"method":"actZoom","params":["in","start"],"id":2,"version":"1.0"}`},
	}
	for _, tt := range tests {
		b, err := tt.req.MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != tt.want {
			t.Errorf("got  %s\nwant %s", b, tt.want)
		}
	}
}

func TestDecodeResponseResult(t *testing.T) {
	r, err := DecodeResponse([]byte(`{"result":[["getVersions","getMethodTypes","startRecMode"]],"id":1}`))
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.StringList()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"getVersions", "getMethodTypes", "startRecMode"}; !reflect.DeepEqual(got, want) {
		t.Errorf("StringList = %v, want %v", got, want)
	}
}

func TestDecodeResponseResults(t *testing.T) {
	raw := `{"results":[["getVersions",[],["string*"],"1.0"],["getEvent",["bool"],["{\"type\":\"string\"}*"],"1.0"],["getEvent",["bool"],["json*"],"1.3"]],"id":2}`
	r, err := DecodeResponse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.MethodTypes()
	if err != nil {
		t.Fatal(err)
	}
	want := []MethodType{{"getVersions", "1.0"}, {"getEvent", "1.0"}, {"getEvent", "1.3"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("MethodTypes = %v, want %v", got, want)
	}
}

func TestDecodeResponseError(t *testing.T) {
	tests := []struct {
		raw  string
		code int
		msg  string
		name string
	}{
		{`{"error":[40401,"Camera Not Ready"],"id":3}`, 40401, "Camera Not Ready", "Camera Not Ready"},
		{`{"error":[12,""],"id":4}`, 12, "", "No Such Method"},
		{`{"error":[1],"id":5}`, 1, "", "Any"},
		{`{"error":[99999,"weird"],"id":6}`, 99999, "weird", ""},
	}
	for _, tt := range tests {
		r, err := DecodeResponse([]byte(tt.raw))
		var rpcErr *Error
		if !errors.As(err, &rpcErr) {
			t.Fatalf("%s: err = %v, want *Error", tt.raw, err)
		}
		if rpcErr.Code != tt.code || rpcErr.Message != tt.msg || rpcErr.Name() != tt.name {
			t.Errorf("%s: got code %d msg %q name %q", tt.raw, rpcErr.Code, rpcErr.Message, rpcErr.Name())
		}
		if r == nil || r.Error != rpcErr {
			t.Errorf("%s: response not returned alongside its error", tt.raw)
		}
		if !strings.Contains(err.Error(), "40401") && tt.code == 40401 {
			t.Errorf("Error() = %q lacks the code", err.Error())
		}
	}
}

func TestDecodeResponseMalformed(t *testing.T) {
	for _, raw := range []string{``, `not json`, `{"id":1}`, `{"error":"x","id":1}`, `{"error":[],"id":1}`} {
		if _, err := DecodeResponse([]byte(raw)); err == nil {
			t.Errorf("%q: want error", raw)
		} else if errors.As(err, new(*Error)) {
			t.Errorf("%q: malformed envelope reported as a camera error: %v", raw, err)
		}
	}
}

func TestClientCall(t *testing.T) {
	var gotBody, gotPath, gotType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody, gotPath, gotType = string(b), r.URL.Path, r.Header.Get("Content-Type")
		switch {
		case strings.Contains(gotBody, `"startMovieRec"`):
			io.WriteString(w, `{"error":[40401,"Camera Not Ready"],"id":2}`)
		case strings.Contains(gotBody, `"boom"`):
			http.Error(w, "nope", http.StatusInternalServerError)
		default:
			io.WriteString(w, `{"result":[["1.0","1.1"]],"id":1}`)
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL + "/sony/camera")

	ex, err := c.Call(context.Background(), "getVersions", "1.0")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/sony/camera" || gotType != "application/json" {
		t.Errorf("POST path %q content-type %q", gotPath, gotType)
	}
	if want := `{"method":"getVersions","params":[],"id":1,"version":"1.0"}`; gotBody != want || string(ex.Request) != want {
		t.Errorf("sent %s, recorded %s, want %s", gotBody, ex.Request, want)
	}
	if string(ex.Response) != `{"result":[["1.0","1.1"]],"id":1}` || ex.Status != 200 {
		t.Errorf("recorded response %d %s", ex.Status, ex.Response)
	}
	if v, _ := ex.Decoded.StringList(); !reflect.DeepEqual(v, []string{"1.0", "1.1"}) {
		t.Errorf("decoded %v", v)
	}

	ex, err = c.Call(context.Background(), "startMovieRec", "1.0")
	var rpcErr *Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != 40401 {
		t.Errorf("err = %v, want camera error 40401", err)
	}
	if !strings.Contains(string(ex.Request), `"id":2`) || !strings.Contains(string(ex.Response), "40401") {
		t.Errorf("exchange not recorded on camera error: %s -> %s", ex.Request, ex.Response)
	}

	ex, err = c.Call(context.Background(), "boom", "1.0")
	if err == nil || ex.Status != 500 || !strings.Contains(string(ex.Response), "nope") {
		t.Errorf("HTTP 500: err %v, status %d, body %q", err, ex.Status, ex.Response)
	}
}

// getEvent v1.0 result layout per Sony's SimpleCameraEventObserver.java:
// [0] availableApiList, [1] cameraStatus, [2] zoomInformation,
// [3] liveviewStatus, [10] storageInformation (array), [21] shootMode.
const eventFixture = `{"id":9,"result":[
 {"type":"availableApiList","names":["getVersions","startMovieRec","actZoom"]},
 {"type":"cameraStatus","cameraStatus":"MovieRecording"},
 {"type":"zoomInformation","zoomPosition":42,"zoomNumberBox":1,"zoomIndexCurrentBox":0,"zoomPositionCurrentBox":42},
 {"type":"liveviewStatus","liveviewStatus":true},
 null,[],null,null,null,null,
 [{"type":"storageInformation","storageID":"Memory Card 1","recordTarget":true,"numberOfRecordableImages":-1,"recordableTime":117}],
 null,null,null,null,null,null,null,null,null,null,
 {"type":"shootMode","currentShootMode":"movie"}
]}`

func TestParseEvent(t *testing.T) {
	r, err := DecodeResponse([]byte(eventFixture))
	if err != nil {
		t.Fatal(err)
	}
	ev, err := r.Event()
	if err != nil {
		t.Fatal(err)
	}
	if ev.CameraStatus != "MovieRecording" || ev.ShootMode != "movie" {
		t.Errorf("status %q shoot mode %q", ev.CameraStatus, ev.ShootMode)
	}
	if !reflect.DeepEqual(ev.APINames, []string{"getVersions", "startMovieRec", "actZoom"}) {
		t.Errorf("APINames = %v", ev.APINames)
	}
	if ev.ZoomPosition == nil || *ev.ZoomPosition != 42 {
		t.Errorf("ZoomPosition = %v", ev.ZoomPosition)
	}
	if _, ok := ev.Types["storageInformation"]; !ok {
		t.Errorf("array-wrapped event element missed: %v", ev.Types)
	}

	empty, err := (&Response{Result: []byte(`[null,{"type":"cameraStatus","cameraStatus":"IDLE"}]`)}).Event()
	if err != nil {
		t.Fatal(err)
	}
	if empty.ZoomPosition != nil || empty.CameraStatus != "IDLE" {
		t.Errorf("sparse event: zoom %v status %q", empty.ZoomPosition, empty.CameraStatus)
	}
}
