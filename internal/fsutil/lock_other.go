//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd || windows)

package fsutil

import "os"

// tryLock does not lock on platforms without flock or LockFileEx: there,
// concurrent writers are not kept apart.
func tryLock(*os.File) (bool, error) { return true, nil }

func unlockFile(*os.File) error { return nil }
