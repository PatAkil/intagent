//go:build unix

package gitx

import (
	"os"
	"syscall"
)

// terminate asks a process to stop. git removes its lock files on SIGTERM.
func terminate(p *os.Process) error { return p.Signal(syscall.SIGTERM) }
