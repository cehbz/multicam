// Package sony is a client for Sony's legacy Camera Remote API: SSDP
// discovery, the device description, JSON-RPC over HTTP and the liveview
// stream.
package sony

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/textproto"
	"strings"
	"time"
)

// SearchTarget is the SSDP search target of the Camera Remote API service.
const SearchTarget = "urn:schemas-sony-com:service:ScalarWebAPI:1"

// SSDPMulticast is the SSDP multicast group and port.
const SSDPMulticast = "239.255.255.250:1900"

// SSDPResponse is a parsed M-SEARCH reply.
type SSDPResponse struct {
	StatusLine string
	Header     http.Header
}

// Location is the device description URL.
func (r *SSDPResponse) Location() string { return r.Header.Get("Location") }

// ParseSSDPResponse parses an M-SEARCH reply. It accepts CRLF or LF line ends
// and a missing terminating blank line, and requires a 200 status and a
// LOCATION header.
func ParseSSDPResponse(b []byte) (*SSDPResponse, error) {
	lines := strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
	status := strings.TrimSpace(lines[0])
	f := strings.Fields(status)
	if len(f) < 2 || !strings.HasPrefix(f[0], "HTTP/") {
		return nil, fmt.Errorf("ssdp: not a response: %q", status)
	}
	if f[1] != "200" {
		return nil, fmt.Errorf("ssdp: status %q", status)
	}
	r := &SSDPResponse{StatusLine: status, Header: http.Header{}}
	for _, line := range lines[1:] {
		if line == "" {
			break
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		r.Header.Add(textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(k)), strings.TrimSpace(v))
	}
	if r.Location() == "" {
		return nil, errors.New("ssdp: response has no LOCATION")
	}
	return r, nil
}

// SSDPReply is one datagram received in answer to an M-SEARCH.
type SSDPReply struct {
	From net.Addr
	Raw  []byte
}

// Search sends an M-SEARCH for SearchTarget to addr (SSDPMulticast on a real
// network) three times, as Sony's sample client does, and collects replies
// for the listen period. Identical datagrams from one sender are kept once.
// It returns the M-SEARCH datagram as sent.
func Search(ctx context.Context, addr string, listen time.Duration) ([]byte, []SSDPReply, error) {
	req := []byte("M-SEARCH * HTTP/1.1\r\n" +
		"HOST: " + SSDPMulticast + "\r\n" +
		"MAN: \"ssdp:discover\"\r\n" +
		"MX: 1\r\n" +
		"ST: " + SearchTarget + "\r\n\r\n")
	dst, err := net.ResolveUDPAddr("udp4", addr)
	if err != nil {
		return req, nil, err
	}
	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return req, nil, err
	}
	defer conn.Close()
	for i := range 3 {
		if i > 0 {
			time.Sleep(100 * time.Millisecond)
		}
		if _, err := conn.WriteTo(req, dst); err != nil {
			return req, nil, fmt.Errorf("ssdp: send: %w", err)
		}
	}

	deadline := time.Now().Add(listen)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	conn.SetReadDeadline(deadline)
	var replies []SSDPReply
	buf := make([]byte, 4096)
	for {
		n, from, err := conn.ReadFrom(buf)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return req, replies, nil
			}
			return req, replies, fmt.Errorf("ssdp: receive: %w", err)
		}
		raw := bytes.Clone(buf[:n])
		dup := false
		for _, r := range replies {
			if r.From.String() == from.String() && bytes.Equal(r.Raw, raw) {
				dup = true
				break
			}
		}
		if !dup {
			replies = append(replies, SSDPReply{From: from, Raw: raw})
		}
	}
}

// DeviceDescription is the part of the UPnP device description the Camera
// Remote API uses.
type DeviceDescription struct {
	FriendlyName string
	Manufacturer string
	ModelName    string
	UDN          string
	APIVersion   string
	Services     []APIService
}

// APIService is one X_ScalarWebAPI_Service entry.
type APIService struct {
	Type          string
	ActionListURL string
	AccessType    string
}

// Endpoint is the JSON-RPC URL of the service: the action list URL joined with
// the service type, e.g. http://192.168.122.1:8080/sony/camera.
func (s APIService) Endpoint() string {
	return strings.TrimSuffix(s.ActionListURL, "/") + "/" + s.Type
}

// Service returns the service of type t, or nil when the device lacks it.
func (d *DeviceDescription) Service(t string) *APIService {
	for i := range d.Services {
		if d.Services[i].Type == t {
			return &d.Services[i]
		}
	}
	return nil
}

// ParseDeviceDescription parses the device description XML fetched from the
// SSDP LOCATION.
func ParseDeviceDescription(b []byte) (*DeviceDescription, error) {
	var doc struct {
		Device struct {
			FriendlyName string `xml:"friendlyName"`
			Manufacturer string `xml:"manufacturer"`
			ModelName    string `xml:"modelName"`
			UDN          string `xml:"UDN"`
			Info         struct {
				Version  string `xml:"X_ScalarWebAPI_Version"`
				Services []struct {
					Type       string `xml:"X_ScalarWebAPI_ServiceType"`
					URL        string `xml:"X_ScalarWebAPI_ActionList_URL"`
					AccessType string `xml:"X_ScalarWebAPI_AccessType"`
				} `xml:"X_ScalarWebAPI_ServiceList>X_ScalarWebAPI_Service"`
			} `xml:"X_ScalarWebAPI_DeviceInfo"`
		} `xml:"device"`
	}
	if err := xml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("device description: %w", err)
	}
	dev := doc.Device
	d := &DeviceDescription{
		FriendlyName: dev.FriendlyName,
		Manufacturer: dev.Manufacturer,
		ModelName:    dev.ModelName,
		UDN:          dev.UDN,
		APIVersion:   strings.TrimSpace(dev.Info.Version),
	}
	for _, s := range dev.Info.Services {
		d.Services = append(d.Services, APIService{
			Type:          strings.TrimSpace(s.Type),
			ActionListURL: strings.TrimSpace(s.URL),
			AccessType:    strings.TrimSpace(s.AccessType),
		})
	}
	if len(d.Services) == 0 {
		return nil, errors.New("device description: no X_ScalarWebAPI services")
	}
	return d, nil
}
