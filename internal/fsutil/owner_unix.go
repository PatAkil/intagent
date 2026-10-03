//go:build unix

package fsutil

import (
	"io/fs"
	"os"
	"syscall"
)

// keepOwner gives f the owner of the file it replaces, where the process may:
// 'sudo intagent init' must not hand a member's files to root.
func keepOwner(f *os.File, old fs.FileInfo) {
	if st, ok := old.Sys().(*syscall.Stat_t); ok && (int(st.Uid) != os.Geteuid() || int(st.Gid) != os.Getegid()) {
		_ = f.Chown(int(st.Uid), int(st.Gid))
	}
}
