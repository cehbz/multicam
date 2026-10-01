package sony

import "syscall"

// bindToDevice binds a socket to a network interface (SO_BINDTODEVICE).
var bindToDevice = syscall.BindToDevice
