//go:build !linux

package mediamtx

import "syscall"

// childAttr is nil on systems without Pdeathsig.
func childAttr() *syscall.SysProcAttr { return nil }
