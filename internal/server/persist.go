package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// writeFileAtomic writes data to path so that a crash leaves either the old
// file or the new one, never a torn mix.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }() // no-op after a successful rename
	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync() // best effort: make the rename durable
		_ = d.Close()
	}
	return nil
}

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

// save writes a snapshot if the board changed since the last one.
func (s *Server) save() error {
	if s.dataDir == "" || s.board.Version() == s.saved {
		return nil
	}
	data, version, err := s.board.Snapshot(s.now())
	if err != nil {
		return err
	}
	if err := writeFileAtomic(s.snapshotPath(), data, 0o600); err != nil {
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
