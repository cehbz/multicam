//go:build !linux

package sony

// bindToDevice binds a socket to a network interface; nil on systems that
// can't.
var bindToDevice func(fd int, iface string) error
