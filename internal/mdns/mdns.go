// Package mdns finds a DNS-SD service instance over multicast DNS, asking for
// unicast answers.
package mdns

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// Port is the mDNS port.
const Port = 5353

// queryInterval is the time between a find's queries.
const queryInterval = time.Second

// unicastResponse is a question's unicast-response (QU) bit, in its class.
const unicastResponse dnsmessage.Class = 1 << 15

// Conn is a socket on the mDNS port: Send multicasts a query, Read reads the
// next message sent to the socket.
type Conn interface {
	Send(b []byte) error
	Read(b []byte) (int, error)
	SetReadDeadline(t time.Time) error
}

// Find returns the address and port of the instance of service (a fully
// qualified name, such as "_http._tcp.local.") whose TXT strings match
// reports true for. It sends a PTR query for service on conn every second,
// and an A query for the instance's target when an answer leaves its address
// out, each asking for a unicast answer, and reads the answers until their
// records, gathered across answers, give the instance's TXT, SRV and target
// address. A query that fails to send waits for the next. It returns ctx's
// error once ctx ends.
func Find(ctx context.Context, conn Conn, service string, match func(txt []string) bool) (netip.AddrPort, error) {
	name, err := dnsmessage.NewName(service)
	if err != nil {
		return netip.AddrPort{}, err
	}
	stop := context.AfterFunc(ctx, func() { conn.SetReadDeadline(time.Unix(1, 0)) })
	defer stop()
	h := heard{service: "." + strings.ToLower(service), txt: map[string][]string{}, srv: map[string]target{}, a: map[string]netip.Addr{}}
	buf := make([]byte, 9000)
	var next time.Time
	for {
		if now := time.Now(); !now.Before(next) {
			conn.Send(query(dnsmessage.TypePTR, name))
			next = now.Add(queryInterval)
		}
		if err := conn.SetReadDeadline(next); err != nil {
			return netip.AddrPort{}, err
		}
		if err := ctx.Err(); err != nil {
			return netip.AddrPort{}, err
		}
		n, err := conn.Read(buf)
		if err := ctx.Err(); err != nil {
			return netip.AddrPort{}, err
		}
		if errors.Is(err, os.ErrDeadlineExceeded) {
			continue
		}
		if err != nil {
			return netip.AddrPort{}, err
		}
		var m dnsmessage.Message
		if err := m.Unpack(buf[:n]); err != nil || !m.Response {
			continue
		}
		h.add(m)
		found, unresolved := h.find(match)
		if found.IsValid() {
			return found, nil
		}
		for _, host := range unresolved {
			if name, err := dnsmessage.NewName(host); err == nil {
				conn.Send(query(dnsmessage.TypeA, name))
			}
		}
	}
}

// query is a query for name's records of type t, asking for a unicast
// answer.
func query(t dnsmessage.Type, name dnsmessage.Name) []byte {
	m := dnsmessage.Message{Questions: []dnsmessage.Question{{Name: name, Type: t, Class: dnsmessage.ClassINET | unicastResponse}}}
	b, err := m.Pack()
	if err != nil {
		panic(err) // a valid name always packs
	}
	return b
}

// target is an SRV record's host and port.
type target struct {
	host string
	port uint16
}

// heard is the records a find has read, by owner name in lower case: the
// TXT strings and SRV targets of service's instances, and hosts' IPv4
// addresses.
type heard struct {
	service string // ".<service>", in lower case
	txt     map[string][]string
	srv     map[string]target
	a       map[string]netip.Addr
}

// add keeps m's records.
func (h heard) add(m dnsmessage.Message) {
	for _, r := range append(append(m.Answers, m.Authorities...), m.Additionals...) {
		owner := strings.ToLower(r.Header.Name.String())
		switch b := r.Body.(type) {
		case *dnsmessage.TXTResource:
			if strings.HasSuffix(owner, h.service) {
				h.txt[owner] = b.TXT
			}
		case *dnsmessage.SRVResource:
			if strings.HasSuffix(owner, h.service) {
				h.srv[owner] = target{host: strings.ToLower(b.Target.String()), port: b.Port}
			}
		case *dnsmessage.AResource:
			h.a[owner] = netip.AddrFrom4(b.A)
		}
	}
}

// find returns the address and port of an instance whose TXT strings match
// reports true for, or else the hosts of such instances whose address is
// unknown.
func (h heard) find(match func(txt []string) bool) (found netip.AddrPort, unresolved []string) {
	for instance, txt := range h.txt {
		t, ok := h.srv[instance]
		if !ok || !match(txt) {
			continue
		}
		if addr, ok := h.a[t.host]; ok {
			return netip.AddrPortFrom(addr, t.port), nil
		}
		unresolved = append(unresolved, t.host)
	}
	return netip.AddrPort{}, unresolved
}
