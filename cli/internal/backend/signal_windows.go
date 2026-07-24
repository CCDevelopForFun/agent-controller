//go:build windows

package backend

import "os"

var interruptSignal os.Signal = os.Interrupt
