//go:build !linux

package link

import (
	"fmt"
	"net/netip"
	"runtime"
)

// New returns a keeper of the phone's links; links need Linux.
func New(Options) (*Keeper, error) {
	return nil, fmt.Errorf("link: camera links are not supported on %s", runtime.GOOS)
}

// Source is the phone's source address on its route to dst; routes are read
// on Linux only.
func Source(netip.Addr) (netip.Addr, error) {
	return netip.Addr{}, fmt.Errorf("link: routes are not read on %s", runtime.GOOS)
}
