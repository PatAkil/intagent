package cli

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/patakil/intagent/internal/board"
	"github.com/patakil/intagent/internal/client"
	"github.com/patakil/intagent/internal/hook"
)

// The unchecked ledger. When the team server does not answer a pre_edit in
// time, the hook lets the edit through and says nothing: a hook failing open
// has nothing to tell. It notes the edit in a ledger for its worktree and
// session instead, and the session's next answered hook that can carry
// context tells the agent which edits went ahead unchecked.
//
// Hooks of one session can run in parallel. Each note is one appended line,
// and a hook takes the ledger by renaming it before reading it, so an edit is
// told once.

const (
	ledgerPrefix = "unchecked-"
	// maxLedger stops a ledger growing through a long outage.
	maxLedger = 256 << 10
	// ledgerTTL: an edit older than this is not news to its session.
	ledgerTTL = 24 * time.Hour
	// orphanAfter: a ledger left by a session that never ended is removed.
	orphanAfter = 7 * 24 * time.Hour
	// tellPaths bounds the paths named.
	tellPaths = 5
)

// errAnsweredLate is the server answering a pre_edit it did not check,
// because the agent would have stopped waiting by the time it did.
var errAnsweredLate = errors.New("the server got to the edit too late to check it")

// uncheckedEdit is one line of a ledger.
type uncheckedEdit struct {
	At    int64    `json:"at"` // Unix seconds
	Paths []string `json:"paths"`
}

// ledgerFile names the ledger of a session in a worktree.
func ledgerFile(root, session string) (string, bool) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256([]byte(root + "\x00" + session))
	return filepath.Join(dir, "intagent", ledgerPrefix+hex.EncodeToString(sum[:8])), true
}

// unanswered reports whether err means the server did not answer: the
// request timed out, met no server, or the server failed.
func unanswered(err error) bool {
	var ae *client.APIError
	if errors.As(err, &ae) {
		return ae.Status >= 500
	}
	return err != nil
}

// noteUnchecked adds an edit that went ahead unchecked to its session's ledger.
func noteUnchecked(root, session string, at time.Time, refs []board.PathRef) {
	p, ok := ledgerFile(root, session)
	if !ok || len(refs) == 0 {
		return
	}
	fi, err := os.Stat(p)
	switch {
	case err == nil && fi.Size() >= maxLedger:
		return
	case err != nil:
		pruneLedgers(filepath.Dir(p), at)
	}
	e := uncheckedEdit{At: at.Unix()}
	for _, r := range refs {
		e.Paths = append(e.Paths, r.Path)
	}
	line, err := json.Marshal(e)
	if err != nil || os.MkdirAll(filepath.Dir(p), 0o700) != nil {
		return
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = f.Write(append(line, '\n')) // one write, so parallel hooks' lines do not mix
	_ = f.Close()
}

// pruneLedgers removes the ledgers of sessions long gone.
func pruneLedgers(dir string, now time.Time) {
	names, _ := filepath.Glob(filepath.Join(dir, ledgerPrefix+"*"))
	for _, n := range names {
		if fi, err := os.Stat(n); err == nil && now.Sub(fi.ModTime()) > orphanAfter {
			_ = os.Remove(n)
		}
	}
}

// claimUnchecked takes a session's ledger, so that no other hook tells it,
// and returns the edits in it that are still news.
func claimUnchecked(root, session string, now time.Time) []uncheckedEdit {
	p, ok := ledgerFile(root, session)
	if !ok {
		return nil
	}
	claimed := fmt.Sprintf("%s.%d.claimed", p, os.Getpid())
	if os.Rename(p, claimed) != nil {
		return nil
	}
	defer func() { _ = os.Remove(claimed) }()
	f, err := os.Open(claimed)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	var out []uncheckedEdit
	sc := bufio.NewScanner(io.LimitReader(f, 2*maxLedger))
	sc.Buffer(nil, maxLedger)
	for sc.Scan() {
		var e uncheckedEdit
		if json.Unmarshal(sc.Bytes(), &e) == nil && len(e.Paths) > 0 && now.Sub(time.Unix(e.At, 0)) < ledgerTTL {
			out = append(out, e)
		}
	}
	return out
}

// dropUnchecked forgets a session's ledger when the session ends.
func dropUnchecked(root, session string) {
	if p, ok := ledgerFile(root, session); ok {
		_ = os.Remove(p)
	}
}

// carriesContext reports whether an agent reads the context in the answer to
// this event.
func carriesContext(ad hook.Adapter, ev hook.Event) bool {
	const probe = "intagent context probe"
	return bytes.Contains(ad.Render(ev, board.HookResult{Decision: board.DecisionAllow, Context: probe}).Stdout, []byte(probe))
}

// renderUnchecked tells an agent which of its edits went ahead unchecked.
func renderUnchecked(edits []uncheckedEdit, url string) string {
	first := edits[0].At
	var paths []string
	for _, e := range edits {
		first = min(first, e.At)
		for _, p := range e.Paths {
			if !slices.Contains(paths, p) {
				paths = append(paths, p)
			}
		}
	}
	when := time.Unix(first, 0).Format("15:04")
	head, them := fmt.Sprintf("%d of this session's edits since %s went", len(edits), when), "them"
	if len(edits) == 1 {
		head = "An edit this session made at " + when + " went"
		if len(paths) == 1 {
			them = "it"
		}
	}
	named := strings.Join(paths[:min(len(paths), tellPaths)], ", ")
	if more := len(paths) - tellPaths; more > 0 {
		named += fmt.Sprintf(" and %d more", more)
	}
	return fmt.Sprintf("[intagent] %s ahead without a check: the team's intagent server at %s did not answer in time. "+
		"A teammate may be working on %s. Check %s with the intagent check_paths tool, or tell your user.", head, url, named, them)
}
