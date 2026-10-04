//go:build !unix

package gitx

import "os"

// terminate stops a process. Windows has no signal for a process to catch,
// so git is killed, as exec.CommandContext does by default.
func terminate(p *os.Process) error { return p.Kill() }
