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
// the footprint is the worktree's. A session's end does not scan when a
// footprint of the worktree reached the server from a scan that began
// within footprintEvery, and after the last shell command that did not
// scan: the Stop before it has just sent one, and at a session's end git
// has little time.
func scanDue(root string, kind board.Kind, now time.Time) bool {
	switch kind {
	case board.KindToolEnd:
		if footprintDue(root, now) {
			return true
		}
		// The scan of this window may have run before this command changed
		// anything, so a session's end must not rely on it.
		stamp(root, "unscanned-", now)
		return false
	case board.KindSessionEnd:
		return !sentRecently(root, now)
	}
	return true
}

// cacheDir is intagent's directory in the user's cache, where the hook log,
// the stamps that pace scans, the index copies they keep and the ledgers of
// edits that went ahead unchecked live, or "" if there is none. An absolute
// XDG_CACHE_HOME is honoured on every system, macOS included, as most
// command-line tools there do; otherwise it is the system's own cache.
func cacheDir() string {
	dir := os.Getenv("XDG_CACHE_HOME")
	if !filepath.IsAbs(dir) {
		var err error
		if dir, err = os.UserCacheDir(); err != nil {
			return ""
		}
	}
	dir = filepath.Join(dir, "intagent")
	// G703: XDG_CACHE_HOME is the user's own setting, read by the hook the
	// user runs, as every XDG tool reads it; it grants nothing the user lacks.
	if os.MkdirAll(dir, 0o700) != nil { //nolint:gosec // G703: see above.
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
	dir := cacheDir()
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
// and sent and unscanned stamps footprintEvery old.
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
		case strings.HasPrefix(name, "sent-"), strings.HasPrefix(name, "unscanned-"):
			fi, err := e.Info()
			stale = err == nil && now.Sub(fi.ModTime()) >= footprintEvery
		}
		if stale {
			_ = os.Remove(filepath.Join(dir, name)) // another hook may have removed it first
		}
	}
}

// footprintSent records that a footprint of the worktree whose scan began
// at began reached the server.
func footprintSent(root string, began time.Time) {
	stamp(root, "sent-", began)
}

// stamp sets the worktree's stamp named by prefix to at.
func stamp(root, prefix string, at time.Time) {
	if dir := cacheDir(); dir != "" {
		p := filepath.Join(dir, prefix+worktreeKey(root))
		if os.WriteFile(p, nil, 0o600) == nil {
			_ = os.Chtimes(p, at, at) // the write's own time is near enough
		}
	}
}

// sentRecently reports whether a footprint of the worktree reached the
// server whose scan began less than footprintEvery before now and after the
// last shell command that did not scan.
func sentRecently(root string, now time.Time) bool {
	dir := cacheDir()
	if dir == "" {
		return false
	}
	sent, err := os.Stat(filepath.Join(dir, "sent-"+worktreeKey(root)))
	if err != nil {
		return false
	}
	if age := now.Sub(sent.ModTime()); age < 0 || age >= footprintEvery {
		return false
	}
	unscanned, err := os.Stat(filepath.Join(dir, "unscanned-"+worktreeKey(root)))
	return err != nil || unscanned.ModTime().Before(sent.ModTime())
}
