package mdns

import (
	"context"
	"net"
	"syscall"

	"golang.org/x/net/ipv4"
	"golang.org/x/sys/unix"
)

// group is the mDNS IPv4 multicast group.
var group = &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: Port}

// Socket is a UDP socket on the mDNS port, shared with the system's other
// listeners there, that multicasts its queries on one interface.
type Socket struct {
	*net.UDPConn
	iface string
}

// Listen opens a socket on 0.0.0.0 at the mDNS port that multicasts on iface.
func Listen(iface string) (*Socket, error) {
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var err error
		cerr := c.Control(func(fd uintptr) {
			if err = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err == nil {
				err = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
			}
		})
		if cerr != nil {
			return cerr
		}
		return err
	}}
	c, err := lc.ListenPacket(context.Background(), "udp4", "0.0.0.0:5353")
	if err != nil {
		return nil, err
	}
	return &Socket{UDPConn: c.(*net.UDPConn), iface: iface}, nil
}

// Send multicasts b to the mDNS group on the socket's interface, looked up
// at each send.
func (s *Socket) Send(b []byte) error {
	ifi, err := net.InterfaceByName(s.iface)
	if err != nil {
		return err
	}
	if err := ipv4.NewPacketConn(s.UDPConn).SetMulticastInterface(ifi); err != nil {
		return err
	}
	_, err = s.WriteTo(b, group)
	return err
}
