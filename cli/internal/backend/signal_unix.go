//go:build !windows

package backend

import "syscall"

var interruptSignal = syscall.SIGINT
