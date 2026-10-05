package mdns

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

const service = "_http._tcp.local."

// The records of Blackmagic Camera's answer on the bridge phone, with another
// instance of the service beside it.
var (
	instance  = dnsmessage.MustNewName("b722b465-4dc9-4e5d-bb76-a30055bf6a72._http._tcp.local.")
	host      = dnsmessage.MustNewName("Android_6NDDURH5.local.")
	other     = dnsmessage.MustNewName("printer._http._tcp.local.")
	otherHost = dnsmessage.MustNewName("printer.local.")
)

func record(name dnsmessage.Name, body dnsmessage.ResourceBody) dnsmessage.Resource {
	return dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Name: name, Class: dnsmessage.ClassINET | 1<<15, TTL: 120}, Body: body}
}

func ptr(to dnsmessage.Name) dnsmessage.Resource {
	return record(dnsmessage.MustNewName(service), &dnsmessage.PTRResource{PTR: to})
}

func txt(name dnsmessage.Name, s ...string) dnsmessage.Resource {
	return record(name, &dnsmessage.TXTResource{TXT: s})
}

func srv(name, target dnsmessage.Name, port uint16) dnsmessage.Resource {
	return record(name, &dnsmessage.SRVResource{Target: target, Port: port})
}

func a(name dnsmessage.Name, addr string) dnsmessage.Resource {
	return record(name, &dnsmessage.AResource{A: netip.MustParseAddr(addr).As4()})
}

var appTXT = []string{"device name=Google Pixel 9 Pro XL", "capabilities=cameraControl", "txtvers=1",
	"unique id=b722b4654dc94e5dbb76a30055bf6a72", "path=/control/api/v1", "camera name=A"}

func isApp(txt []string) bool {
	for _, s := range txt {
		if s == "unique id=b722b4654dc94e5dbb76a30055bf6a72" {
			return true
		}
	}
	return false
}

// responder is a fake mDNS responder on loopback: it answers each query it
// gets with answer's records for that query, none when answer gives none,
// and keeps the queries.
type responder struct {
	conn    *net.UDPConn
	queries chan dnsmessage.Message
}

func newResponder(t *testing.T, answer func(q dnsmessage.Message) []dnsmessage.Resource) *responder {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	r := &responder{conn: conn, queries: make(chan dnsmessage.Message, 100)}
	go func() {
		buf := make([]byte, 9000)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			var q dnsmessage.Message
			if err := q.Unpack(buf[:n]); err != nil {
				continue
			}
			r.queries <- q
			records := answer(q)
			if records == nil {
				continue
			}
			m := dnsmessage.Message{Header: dnsmessage.Header{Response: true, Authoritative: true}, Answers: records}
			b, err := m.Pack()
			if err != nil {
				panic(err)
			}
			conn.WriteToUDP(b, from)
		}
	}()
	return r
}

// query is the next query the responder got.
func (r *responder) query(t *testing.T) dnsmessage.Message {
	t.Helper()
	select {
	case q := <-r.queries:
		return q
	case <-time.After(3 * time.Second):
		t.Fatal("no query within 3 s")
		return dnsmessage.Message{}
	}
}

// loopConn is a Conn on loopback whose queries go to a responder; the first
// fails sends fail.
type loopConn struct {
	*net.UDPConn
	to    *net.UDPAddr
	fails atomic.Int32
}

func (c *loopConn) Send(b []byte) error {
	if c.fails.Add(-1) >= 0 {
		return errors.New("sendto: network is unreachable")
	}
	_, err := c.WriteToUDP(b, c.to)
	return err
}

func dial(t *testing.T, r *responder) *loopConn {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return &loopConn{UDPConn: conn, to: r.conn.LocalAddr().(*net.UDPAddr)}
}

// question is q's only question, failing unless it has one.
func question(t *testing.T, q dnsmessage.Message) dnsmessage.Question {
	t.Helper()
	if q.Response || len(q.Questions) != 1 {
		t.Fatalf("query %+v, want one question", q)
	}
	return q.Questions[0]
}

func TestFindAsksForUnicastAnswersToThePTRQuery(t *testing.T) {
	r := newResponder(t, func(dnsmessage.Message) []dnsmessage.Resource { return nil })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go Find(ctx, dial(t, r), service, isApp)
	q := question(t, r.query(t))
	want := dnsmessage.Question{Name: dnsmessage.MustNewName(service), Type: dnsmessage.TypePTR, Class: dnsmessage.ClassINET | 1<<15}
	if q != want {
		t.Errorf("question %+v, want %+v", q, want)
	}
}

func TestFindReturnsTheMatchingInstancesAddressAndPort(t *testing.T) {
	r := newResponder(t, func(dnsmessage.Message) []dnsmessage.Resource {
		return []dnsmessage.Resource{
			ptr(other), txt(other, "unique id=0000"), srv(other, otherHost, 80), a(otherHost, "192.168.1.5"),
			ptr(instance), txt(instance, appTXT...), srv(instance, host, 4444), a(host, "192.168.1.111"),
		}
	})
	got, err := Find(soon(t), dial(t, r), service, isApp)
	if want := netip.MustParseAddrPort("192.168.1.111:4444"); err != nil || got != want {
		t.Errorf("Find = %v, %v; want %v", got, err, want)
	}
}

func TestFindAsksForTheTargetsAddressWhenTheAnswerLacksIt(t *testing.T) {
	r := newResponder(t, func(q dnsmessage.Message) []dnsmessage.Resource {
		if q.Questions[0].Type == dnsmessage.TypeA && strings.EqualFold(q.Questions[0].Name.String(), host.String()) {
			return []dnsmessage.Resource{a(host, "192.168.1.111")}
		}
		return []dnsmessage.Resource{ptr(instance), txt(instance, appTXT...), srv(instance, host, 4444)}
	})
	got, err := Find(soon(t), dial(t, r), service, isApp)
	if want := netip.MustParseAddrPort("192.168.1.111:4444"); err != nil || got != want {
		t.Fatalf("Find = %v, %v; want %v", got, err, want)
	}
	r.query(t) // the PTR query
	q := question(t, r.query(t))
	if q.Type != dnsmessage.TypeA || q.Class != dnsmessage.ClassINET|1<<15 {
		t.Errorf("second question %+v, want the target's A with the unicast-response bit", q)
	}
}

func TestFindQueriesEverySecondUntilTheInstanceAnswers(t *testing.T) {
	var queries atomic.Int32
	r := newResponder(t, func(dnsmessage.Message) []dnsmessage.Resource {
		if queries.Add(1) == 1 {
			return []dnsmessage.Resource{ptr(other), txt(other, "unique id=0000"), srv(other, otherHost, 80), a(otherHost, "192.168.1.5")}
		}
		return []dnsmessage.Resource{ptr(instance), txt(instance, appTXT...), srv(instance, host, 4444), a(host, "192.168.1.111")}
	})
	start := time.Now()
	got, err := Find(soon(t), dial(t, r), service, isApp)
	if want := netip.MustParseAddrPort("192.168.1.111:4444"); err != nil || got != want {
		t.Fatalf("Find = %v, %v; want %v", got, err, want)
	}
	if took := time.Since(start); took < 900*time.Millisecond || took > 2*time.Second {
		t.Errorf("found after %v, want the second query's answer about a second in", took)
	}
}

func TestFindIgnoresQueriesAndOtherServicesInstances(t *testing.T) {
	r := newResponder(t, func(dnsmessage.Message) []dnsmessage.Resource { return nil })
	conn := dial(t, r)
	from, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer from.Close()
	elsewhere := dnsmessage.MustNewName("b722b465-4dc9-4e5d-bb76-a30055bf6a72._ipp._tcp.local.")
	for _, m := range []dnsmessage.Message{
		{Questions: []dnsmessage.Question{{Name: instance, Type: dnsmessage.TypeTXT, Class: dnsmessage.ClassINET}},
			Answers: []dnsmessage.Resource{txt(instance, appTXT...), srv(instance, host, 4444), a(host, "192.168.1.9")}},
		{Header: dnsmessage.Header{Response: true},
			Answers: []dnsmessage.Resource{txt(elsewhere, appTXT...), srv(elsewhere, host, 631), a(host, "192.168.1.9")}},
	} {
		b, err := m.Pack()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := from.WriteToUDP(b, conn.LocalAddr().(*net.UDPAddr)); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	if got, err := Find(ctx, conn, service, isApp); err != context.DeadlineExceeded {
		t.Errorf("Find = %v, %v; want nothing found", got, err)
	}
}

func TestFindRetriesAFailedSendAtTheNextTick(t *testing.T) {
	r := newResponder(t, func(dnsmessage.Message) []dnsmessage.Resource {
		return []dnsmessage.Resource{ptr(instance), txt(instance, appTXT...), srv(instance, host, 4444), a(host, "192.168.1.111")}
	})
	conn := dial(t, r)
	conn.fails.Store(1)
	start := time.Now()
	got, err := Find(soon(t), conn, service, isApp)
	if want := netip.MustParseAddrPort("192.168.1.111:4444"); err != nil || got != want {
		t.Fatalf("Find = %v, %v; want %v", got, err, want)
	}
	if took := time.Since(start); took < 900*time.Millisecond {
		t.Errorf("found after %v, want the next tick's query", took)
	}
}

func TestFindEndsWithItsContext(t *testing.T) {
	r := newResponder(t, func(dnsmessage.Message) []dnsmessage.Resource { return nil })
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := Find(ctx, dial(t, r), service, isApp)
		done <- err
	}()
	r.query(t)
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Errorf("Find = %v, want %v", err, context.Canceled)
		}
	case <-time.After(200 * time.Millisecond):
		t.Error("Find still running 200 ms after its context ended")
	}
}

func TestListenSharesThePort(t *testing.T) {
	first, err := Listen("lo")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Listen("lo")
	if err != nil {
		t.Fatalf("second Listen: %v", err)
	}
	defer second.Close()
	if got := first.LocalAddr().(*net.UDPAddr).Port; got != Port {
		t.Errorf("port %d, want %d", got, Port)
	}
}

func TestSendOnAMissingInterfaceFails(t *testing.T) {
	s, err := Listen("nosuch0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Send([]byte{0}); err == nil {
		t.Error("Send on a missing interface succeeded")
	}
}

// soon is a context that ends 3 s from now.
func soon(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	t.Cleanup(cancel)
	return ctx
}
