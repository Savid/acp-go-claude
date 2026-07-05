//go:build unix && !linux

package claude

import "syscall"

// Pdeathsig is Linux-only; other unix platforms rely on process-group
// signalling alone.
func unixSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
