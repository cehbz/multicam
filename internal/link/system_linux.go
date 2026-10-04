package link

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4/nclient4"
	"github.com/mdlayher/genetlink"
	"github.com/mdlayher/netlink"
	rtnl "github.com/vishvananda/netlink"
)

// rulePriority is the priority of each link's routing rule: below netd's
// rules, which end in unreachable at 32000.
const rulePriority = 9001

// nl80211 commands and attributes (linux/nl80211.h).
const (
	nl80211NewInterface  = 7
	nl80211DelInterface  = 8
	nl80211AttrWiphy     = 1
	nl80211AttrIfindex   = 3
	nl80211AttrIfname    = 4
	nl80211AttrIftype    = 5
	nl80211IftypeStation = 2
)

// New returns a keeper of the phone's links.
func New(o Options) (*Keeper, error) {
	if o.Dir == "" {
		return nil, errors.New("link: no directory for the supplicants' sockets")
	}
	o = o.withDefaults()
	sys := System{Phy: o.Phy}
	return newKeeper(sys, &WPA{Bin: o.Supplicant, Lib: o.Lib, Dir: o.Dir}, sys, Lease), nil
}

// System is the phone's network stack: the radio's interfaces over nl80211,
// addresses, routes and rules over rtnetlink.
type System struct {
	Phy string // the radio interfaces are added to, such as phy0
}

func (System) Exists(name string) (bool, error) {
	_, err := rtnl.LinkByName(name)
	if _, ok := errors.AsType[rtnl.LinkNotFoundError](err); ok {
		return false, nil
	}
	return err == nil, err
}

// Add creates a station interface on the radio.
func (s System) Add(name string) error {
	b, err := os.ReadFile("/sys/class/ieee80211/" + s.Phy + "/index")
	if err != nil {
		return err
	}
	phy, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 32)
	if err != nil {
		return fmt.Errorf("%s index: %w", s.Phy, err)
	}
	return nl80211(nl80211NewInterface, func(ae *netlink.AttributeEncoder) {
		ae.Uint32(nl80211AttrWiphy, uint32(phy))
		ae.String(nl80211AttrIfname, name)
		ae.Uint32(nl80211AttrIftype, nl80211IftypeStation)
	})
}

func (System) Delete(name string) error {
	l, err := rtnl.LinkByName(name)
	if err != nil {
		return err
	}
	return nl80211(nl80211DelInterface, func(ae *netlink.AttributeEncoder) {
		ae.Uint32(nl80211AttrIfindex, uint32(l.Attrs().Index))
	})
}

// nl80211 sends one nl80211 command and waits for its acknowledgement.
func nl80211(cmd uint8, attrs func(*netlink.AttributeEncoder)) error {
	c, err := genetlink.Dial(nil)
	if err != nil {
		return err
	}
	defer c.Close()
	fam, err := c.GetFamily("nl80211")
	if err != nil {
		return err
	}
	ae := netlink.NewAttributeEncoder()
	attrs(ae)
	data, err := ae.Encode()
	if err != nil {
		return err
	}
	_, err = c.Execute(genetlink.Message{Header: genetlink.Header{Command: cmd, Version: fam.Version}, Data: data},
		fam.ID, netlink.Request|netlink.Acknowledge)
	return err
}

func (System) Addr(iface string) (netip.Prefix, error) {
	l, err := rtnl.LinkByName(iface)
	if err != nil {
		return netip.Prefix{}, err
	}
	addrs, err := rtnl.AddrList(l, rtnl.FAMILY_V4)
	if err != nil || len(addrs) == 0 {
		return netip.Prefix{}, err
	}
	ip, _ := netip.AddrFromSlice(addrs[0].IP.To4())
	ones, _ := addrs[0].Mask.Size()
	return netip.PrefixFrom(ip, ones), nil
}

func (System) SetAddr(iface string, p netip.Prefix) error {
	l, err := rtnl.LinkByName(iface)
	if err != nil {
		return err
	}
	return rtnl.AddrReplace(l, &rtnl.Addr{IPNet: &net.IPNet{
		IP: p.Addr().AsSlice(), Mask: net.CIDRMask(p.Bits(), 32),
	}})
}

// Route puts dst on iface's link in table, and a rule sending sockets bound
// to iface to that table.
func (s System) Route(iface string, dst netip.Addr, table int) error {
	l, err := rtnl.LinkByName(iface)
	if err != nil {
		return err
	}
	if err := rtnl.RouteReplace(&rtnl.Route{
		LinkIndex: l.Attrs().Index,
		Dst:       &net.IPNet{IP: dst.AsSlice(), Mask: net.CIDRMask(32, 32)},
		Scope:     rtnl.SCOPE_LINK,
		Table:     table,
	}); err != nil {
		return err
	}
	if err := s.Unroute(iface); err != nil {
		return err
	}
	r := rule(iface)
	r.Table = table
	return rtnl.RuleAdd(r)
}

// Unroute deletes iface's rule. Its route goes with the interface.
func (System) Unroute(iface string) error {
	for {
		err := rtnl.RuleDel(rule(iface))
		if errors.Is(err, syscall.ENOENT) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func rule(iface string) *rtnl.Rule {
	r := rtnl.NewRule()
	r.Family = rtnl.FAMILY_V4
	r.Priority = rulePriority
	r.OifName = iface
	return r
}

// Lease leases an address for iface over DHCP.
func Lease(ctx context.Context, iface string) (netip.Prefix, error) {
	c, err := nclient4.New(iface, nclient4.WithTimeout(time.Second), nclient4.WithRetry(8))
	if err != nil {
		return netip.Prefix{}, err
	}
	defer c.Close()
	l, err := c.Request(ctx)
	if err != nil {
		return netip.Prefix{}, err
	}
	ip, ok := netip.AddrFromSlice(l.ACK.YourIPAddr.To4())
	if !ok {
		return netip.Prefix{}, fmt.Errorf("leased %v", l.ACK.YourIPAddr)
	}
	bits := 24
	if m := l.ACK.SubnetMask(); m != nil {
		bits, _ = m.Size()
	}
	return netip.PrefixFrom(ip, bits), nil
}
