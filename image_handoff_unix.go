//go:build unix

package claudeacp

import "syscall"

// handoffOpenFlags keeps the one open of a host-named file from blocking.
// os.Root confines the path, but it says nothing about FIFOs or device files, so
// O_NONBLOCK is what stops a FIFO with no writer from parking the turn's
// goroutine inside open(2) where no cancellation can reach it. The descriptor's
// mode is then what refuses it.
const handoffOpenFlags = syscall.O_NONBLOCK
