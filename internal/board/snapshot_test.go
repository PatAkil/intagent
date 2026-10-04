package board

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"
)

// Snapshots share the claims' footprints with the board, and are encoded
// after the lock is released, so a footprint is copied on write. Eight
// writers change footprints in every way a hook can (a new file, a newer
// touch, an area that moves, a footprint replaced) while snapshots are taken
// and restored; under -race the detector must find nothing.
func TestSnapshotsShareFootprintsWithoutRaces(t *testing.T) {
	shareFootprintsUnderLoad(t, "")
}

// raceControl names the environment variable that has
// TestSnapshotSharingControl play the control in a process of its own.
const raceControl = "INTAGENT_TEST_RACE_CONTROL"

// The control for the test above: there, a claim that forgot its footprint
// was shared, and so changed it in place as a missed copy on write would,
// must make the race detector report it. Otherwise the test above could not
// fail. The control runs in a process of its own, which must fail.
func TestSnapshotSharingControl(t *testing.T) {
	if forget := os.Getenv(raceControl); forget != "" {
		shareFootprintsUnderLoad(t, forget)
		return
	}
	if !raceEnabled {
		t.Skip("the control needs the race detector (go test -race)")
	}
	for forget, writer := range map[string]string{"footprint": "putTouch", "alerted": "alert"} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestSnapshotSharingControl$", "-test.count=1")
		cmd.Env = append(os.Environ(), raceControl+"="+forget)
		out, err := cmd.CombinedOutput()
		if err == nil || !bytes.Contains(out, []byte("WARNING: DATA RACE")) || !bytes.Contains(out, []byte("(*claim)."+writer+"(")) {
			t.Errorf("writing a shared %s in place went unreported (err %v):\n%s", forget, err, out)
		}
	}
}

// shareFootprintsUnderLoad runs eight writers against snapshots and
// restores. With forget set, a ninth goroutine keeps clearing every claim's
// mark that a snapshot shares its footprint, or its alerts.
func shareFootprintsUnderLoad(t *testing.T, forget string) {
	h := newHarness(t)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m := fmt.Sprintf("m%d", g)
			for i := range 150 {
				now := t0.Add(time.Duration(i) * time.Second)
				file := PathRef{Path: fmt.Sprintf("p/%d.go", i%9), Area: fmt.Sprintf("p%d", i%4)}
				_, _ = h.b.Hook(now, HookEvent{Kind: KindPostEdit, Member: m, Agent: AgentCodex, SessionID: "s", Where: whereOf(m),
					Paths: []PathRef{file}})
				if i%10 == 9 {
					_, _ = h.b.Hook(now, HookEvent{Kind: KindHeartbeat, Member: m, Agent: AgentCodex, SessionID: "s", Where: whereOf(m),
						Footprint: &Footprint{Files: []PathRef{file, {Path: "q/1.go", Area: "q"}}}})
				}
			}
		}()
	}
	if forget != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				h.b.mu.Lock()
				for _, c := range h.b.claims {
					if forget == "footprint" {
						c.fpShared = false
					} else {
						c.alertedShared = false
					}
				}
				h.b.mu.Unlock()
			}
		}()
	}
	for i := range 60 {
		data, _, err := h.b.Snapshot(t0)
		if err != nil {
			t.Fatal(err)
		}
		if i%10 == 9 {
			if err := h.b.Restore(data); err != nil {
				t.Fatal(err)
			}
		}
	}
	close(stop)
	wg.Wait()
	if err := h.b.checkIndexes(); err != nil {
		t.Fatal(err)
	}
}

// The copy a snapshot takes under the lock does not grow with the files the
// claims changed: it shares their footprints, the alerts they heard about
// the files and the files their inbox items name. It allocated a touch for
// each file, and copied the rest.
func TestSnapshotCopyDoesNotGrowWithFootprints(t *testing.T) {
	allocs := func(files int) float64 {
		h := newHarness(t)
		for m := range 20 {
			fp := &Footprint{}
			for f := range files {
				fp.Files = append(fp.Files, PathRef{Path: fmt.Sprintf("a%d/f%d.go", f%7, f), Area: fmt.Sprintf("a%d", f%7)})
			}
			// The same files for all, so each tells the others about all of them.
			member := fmt.Sprintf("m%d", m)
			if _, err := h.b.Hook(t0, HookEvent{Kind: KindHeartbeat, Member: member, Agent: AgentCodex, SessionID: "s",
				Where: whereOf(member), Footprint: fp}); err != nil {
				t.Fatal(err)
			}
		}
		return testing.AllocsPerRun(5, func() { h.b.snapshotCopy(t0) })
	}
	if one, many := allocs(1), allocs(500); many > one {
		t.Fatalf("a snapshot's copy allocates %.0f times with 500 files a claim, %.0f with one", many, one)
	}
}
