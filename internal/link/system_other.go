//go:build !linux

package link

import (
	"fmt"
	"runtime"
)

// New returns a keeper of the phone's links; links need Linux.
func New(Options) (*Keeper, error) {
	return nil, fmt.Errorf("link: camera links are not supported on %s", runtime.GOOS)
}
