package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/patakil/intagent/internal/board"
)

// footprintEvery paces the footprint scans a worktree's hooks make: on a
// very large monorepo one takes a noticeable fraction of a second of CPU.
const footprintEvery = 15 * time.Second

// scanDue reports whether a hook of kind should scan the worktree at root
// for its footprint at now. A shell command scans at most once per
// footprintEvery window of the worktree, whichever of its sessions ran it:
// the footprint is the worktree's. A session's end does not scan when the
// worktree's footprint reached the server within footprintEvery: the Stop
// before it has just sent one, and at a session's end git has little time.
func scanDue(root string, kind board.Kind, now time.Time) bool {
	switch kind {
	case board.KindToolEnd:
		return footprintDue(root, now)
	case board.KindSessionEnd:
		return !sentRecently(root, now)
	}
	return true
}

// stampDir is where the stamps that pace scans live, or "" if there is none.
func stampDir() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	dir = filepath.Join(dir, "intagent")
	if os.MkdirAll(dir, 0o700) != nil {
		return ""
	}
	return dir
}

// worktreeKey names a worktree in stamp file names.
func worktreeKey(root string) string {
	sum := sha256.Sum256([]byte(root))
	return hex.EncodeToString(sum[:8])
}

// footprintDue claims the current footprintEvery window of the worktree at
// root for a scan after a shell command: the first hook in the window
// creates its stamp and scans, and the others find it and do not. A stamp
// is named by its window, so one left by a killed hook expires with it. The
// hook that claims a window removes the stamps of windows past. Any trouble
// with the stamps means a scan.
func footprintDue(root string, now time.Time) bool {
	dir := stampDir()
	if dir == "" {
		return true
	}
	prefix := "scan-" + worktreeKey(root) + "-"
	window := now.UnixNano() / int64(footprintEvery)
	f, err := os.OpenFile(filepath.Join(dir, prefix+strconv.FormatInt(window, 10)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return false
	}
	if err == nil {
		_ = f.Close() // empty
		pruneStamps(dir, window, now)
	}
	return true
}

// pruneStamps removes the scan stamps of windows before window, those of
// earlier versions of intagent, which were named by worktree and session,
// and sent stamps footprintEvery old.
func pruneStamps(dir string, window int64, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		stale := false
		switch {
		case strings.HasPrefix(name, "scan-"):
			_, w, ok := strings.Cut(strings.TrimPrefix(name, "scan-"), "-")
			n, err := strconv.ParseInt(w, 10, 64)
			stale = !ok || err != nil || n < window
		case strings.HasPrefix(name, "sent-"):
			fi, err := e.Info()
			stale = err == nil && now.Sub(fi.ModTime()) >= footprintEvery
		}
		if stale {
			_ = os.Remove(filepath.Join(dir, name)) // another hook may have removed it first
		}
	}
}

// footprintSent records that the worktree's footprint reached the server.
func footprintSent(root string, now time.Time) {
	if dir := stampDir(); dir != "" {
		p := filepath.Join(dir, "sent-"+worktreeKey(root))
		if os.WriteFile(p, nil, 0o600) == nil {
			_ = os.Chtimes(p, now, now) // the write's own time is near enough
		}
	}
}

// sentRecently reports whether the worktree's footprint reached the server
// less than footprintEvery before now.
func sentRecently(root string, now time.Time) bool {
	dir := stampDir()
	if dir == "" {
		return false
	}
	fi, err := os.Stat(filepath.Join(dir, "sent-"+worktreeKey(root)))
	if err != nil {
		return false
	}
	age := now.Sub(fi.ModTime())
	return age >= 0 && age < footprintEvery
}
