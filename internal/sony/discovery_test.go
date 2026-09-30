package sony

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

// testdata/ssdp-response.txt is the camera M-SEARCH reply captured in
// github.com/arcatdmz/node-sonycam (MIT), stored with LF line ends.
// testdata/dd-DSC-HX400V.xml is the device description fixture from
// github.com/Bloodevil/sony_camera_api (pysony, MIT).

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseSSDPResponse(t *testing.T) {
	lf := string(readFixture(t, "ssdp-response.txt"))
	crlf := strings.ReplaceAll(lf, "\n", "\r\n") + "\r\n"
	for name, raw := range map[string]string{"lf": lf, "crlf": crlf} {
		t.Run(name, func(t *testing.T) {
			r, err := ParseSSDPResponse([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			if got, want := r.Location(), "http://192.168.122.1:64321/dd.xml"; got != want {
				t.Errorf("Location = %q, want %q", got, want)
			}
			if got := r.Header.Get("st"); got != SearchTarget {
				t.Errorf("ST = %q, want %q", got, SearchTarget)
			}
			if got, want := r.Header.Get("USN"), "uuid:000000001000-1010-8000-9AF1701181A3::"+SearchTarget; got != want {
				t.Errorf("USN = %q, want %q", got, want)
			}
			if _, ok := r.Header["Ext"]; !ok {
				t.Errorf("empty EXT header dropped: %v", r.Header)
			}
		})
	}
}

func TestParseSSDPResponseRejects(t *testing.T) {
	for name, raw := range map[string]string{
		"not a response": "M-SEARCH * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\n\r\n",
		"not 200":        "HTTP/1.1 404 Not Found\r\nLOCATION: http://x/dd.xml\r\n\r\n",
		"no location":    "HTTP/1.1 200 OK\r\nST: " + SearchTarget + "\r\n\r\n",
		"empty":          "",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseSSDPResponse([]byte(raw)); err == nil {
				t.Error("want error")
			}
		})
	}
}

func TestSearchSendsMSearchAndCollectsReplies(t *testing.T) {
	responder, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer responder.Close()
	reply := readFixture(t, "ssdp-response.txt")
	got := make(chan string, 8)
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := responder.ReadFrom(buf)
			if err != nil {
				return
			}
			got <- string(buf[:n])
			responder.WriteTo(reply, from)
		}
	}()

	req, replies, err := Search(context.Background(), responder.LocalAddr().String(), 500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	var msearch string
	select {
	case msearch = <-got:
	case <-time.After(time.Second):
		t.Fatal("no M-SEARCH datagram received")
	}
	for _, line := range []string{"M-SEARCH * HTTP/1.1\r\n", "MAN: \"ssdp:discover\"\r\n", "ST: " + SearchTarget + "\r\n"} {
		if !strings.Contains(msearch, line) {
			t.Errorf("M-SEARCH missing %q:\n%s", line, msearch)
		}
	}
	if !strings.HasSuffix(msearch, "\r\n\r\n") {
		t.Errorf("M-SEARCH not terminated by a blank line: %q", msearch)
	}
	if string(req) != msearch {
		t.Errorf("returned request differs from the datagram sent")
	}
	if len(replies) != 1 {
		t.Fatalf("got %d replies, want 1 (identical replies deduplicated)", len(replies))
	}
	if string(replies[0].Raw) != string(reply) {
		t.Errorf("reply raw bytes altered")
	}
}

func TestParseDeviceDescription(t *testing.T) {
	d, err := ParseDeviceDescription(readFixture(t, "dd-DSC-HX400V.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if d.FriendlyName != "DSC-HX400V" || d.ModelName != "SonyImagingDevice" || d.UDN != "uuid:000000001000-1010-8000-B272BF3A6A3F" {
		t.Errorf("device identity = %q %q %q", d.FriendlyName, d.ModelName, d.UDN)
	}
	if d.APIVersion != "1.0" {
		t.Errorf("APIVersion = %q", d.APIVersion)
	}
	var types []string
	for _, s := range d.Services {
		types = append(types, s.Type)
	}
	if got := strings.Join(types, ","); got != "guide,accessControl,camera" {
		t.Errorf("service types = %s", got)
	}
	cam := d.Service("camera")
	if cam == nil {
		t.Fatal("no camera service")
	}
	if cam.ActionListURL != "http://192.168.122.1:8080/sony" {
		t.Errorf("ActionListURL = %q", cam.ActionListURL)
	}
	if got, want := cam.Endpoint(), "http://192.168.122.1:8080/sony/camera"; got != want {
		t.Errorf("Endpoint = %q, want %q", got, want)
	}
	if d.Service("avContent") != nil {
		t.Error("avContent service reported but absent")
	}
}

func TestServiceEndpointTrailingSlash(t *testing.T) {
	s := APIService{Type: "camera", ActionListURL: "http://10.0.0.1:10000/sony/"}
	if got, want := s.Endpoint(), "http://10.0.0.1:10000/sony/camera"; got != want {
		t.Errorf("Endpoint = %q, want %q", got, want)
	}
}

func TestParseDeviceDescriptionRejects(t *testing.T) {
	noAPI := `<?xml version="1.0"?><root xmlns="urn:schemas-upnp-org:device-1-0"><device><friendlyName>x</friendlyName></device></root>`
	for name, raw := range map[string]string{"not xml": "<<<", "no ScalarWebAPI services": noAPI} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseDeviceDescription([]byte(raw)); err == nil {
				t.Error("want error")
			}
		})
	}
}
