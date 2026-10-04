package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/patakil/intagent/internal/fsutil"
)

func (s *Server) snapshotPath() string { return filepath.Join(s.dataDir, "board.json") }

// load restores the board from the data directory, if a snapshot exists.
func (s *Server) load() error {
	if s.dataDir == "" {
		return nil
	}
	data, err := os.ReadFile(s.snapshotPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := s.board.Restore(data); err != nil {
		return fmt.Errorf("%s: %w", s.snapshotPath(), err)
	}
	s.saved = s.board.Version()
	return nil
}

// save writes a snapshot if the board changed since the last one. The
// snapshot is streamed to a temporary file beside the last, which it then
// replaces (fsutil.WriteFileFunc).
func (s *Server) save() error {
	if s.dataDir == "" || s.board.Version() == s.saved {
		return nil
	}
	if err := os.MkdirAll(s.dataDir, 0o700); err != nil {
		return err
	}
	var version uint64
	err := fsutil.WriteFileFunc(s.snapshotPath(), 0o600, func(w io.Writer) error {
		var err error
		version, err = s.board.WriteSnapshot(w, s.now())
		return err
	})
	if err != nil {
		return err
	}
	s.saved = version
	return nil
}

// maintain sweeps the board and saves snapshots until ctx ends, then saves once more.
func (s *Server) maintain(ctx context.Context) {
	sweep := time.NewTicker(s.sweepEvery)
	persist := time.NewTicker(time.Second)
	defer sweep.Stop()
	defer persist.Stop()
	for {
		select {
		case <-ctx.Done():
			if err := s.save(); err != nil {
				s.log.Error("final snapshot failed", "err", err)
			}
			return
		case <-sweep.C:
			s.board.Sweep(s.now())
		case <-persist.C:
			if err := s.save(); err != nil {
				s.log.Error("snapshot failed", "err", err)
			}
		}
	}
}
