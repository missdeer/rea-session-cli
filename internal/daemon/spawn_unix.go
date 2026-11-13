//go:build !windows

package daemon

import "syscall"

// detachedProcAttr starts the daemon in a new session so it survives the
// controlling terminal.
func detachedProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
