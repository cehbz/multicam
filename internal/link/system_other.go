//go:build !linux

package link

import (
	"context"
	"fmt"
	"net/netip"
	"runtime"
)

// New returns a keeper of the phone's links; links need Linux.
func New(Options) (*Keeper, error) {
	return nil, fmt.Errorf("link: camera links are not supported on %s", runtime.GOOS)
}

// AwaitRoute returns at once: routes are watched on Linux only.
func AwaitRoute(context.Context, netip.Addr) error { return nil }
