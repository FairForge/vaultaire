//go:build unix

package drivers

import (
	"errors"
	"syscall"
)

// pidAlive reports whether a process with this id exists (a reused id
// reads as alive: its staging folder is then kept, never someone's taken).
func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
