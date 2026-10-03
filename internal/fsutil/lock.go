package fsutil

import (
	"errors"
	"os"
	"time"
)

// ErrLocked reports that another process held the lock for the whole wait.
var ErrLocked = errors.New("locked by another process")

// Lock takes an exclusive lock on the file at path, creating it if needed,
// waiting up to wait for another holder to let go. The operating system drops
// the lock when its holder exits, so a crash leaves no stale lock to clear.
// The file stays behind when unlocked: removing it would let one process lock
// the old file while another locks a new one of the same name.
func Lock(path string, wait time.Duration) (unlock func(), err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		ok, err := tryLock(f)
		switch {
		case err != nil:
			_ = f.Close()
			return nil, err
		case ok:
			return func() { _ = unlockFile(f); _ = f.Close() }, nil
		case time.Now().After(deadline):
			_ = f.Close()
			return nil, ErrLocked
		}
		time.Sleep(lockPoll)
	}
}

const lockPoll = 20 * time.Millisecond
