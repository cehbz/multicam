package mediamtx

import "syscall"

// childAttr makes the child die with its parent (Pdeathsig).
func childAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
