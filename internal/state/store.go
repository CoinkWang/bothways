package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrCorrupt wraps state files that exist but cannot be parsed.
var ErrCorrupt = errors.New("state.json is corrupt")

// Store reads and writes state.json under Dir ($GIT_DIR/bothways).
type Store struct {
	Dir string
}

func (s *Store) Path() string     { return filepath.Join(s.Dir, "state.json") }
func (s *Store) lockPath() string { return filepath.Join(s.Dir, "lock") }

// Exists reports whether state.json is present.
func (s *Store) Exists() bool {
	_, err := os.Stat(s.Path())
	return err == nil
}

// Load reads the state; a missing file yields an empty state.
func (s *Store) Load() (*State, error) {
	data, err := os.ReadFile(s.Path())
	if errors.Is(err, os.ErrNotExist) {
		return New(), nil
	}
	if err != nil {
		return nil, err
	}
	st := New()
	if err := json.Unmarshal(data, st); err != nil {
		return nil, fmt.Errorf("%w: %v (run `bothways doctor`)", ErrCorrupt, err)
	}
	if st.SchemaVersion > SchemaVersion {
		return nil, fmt.Errorf("state.json schema_version %d is newer than this bothways supports (%d)", st.SchemaVersion, SchemaVersion)
	}
	st.SchemaVersion = SchemaVersion
	st.Normalize()
	return st, nil
}

// Update runs fn on the current state under an exclusive file lock and
// writes the result atomically. The lock covers only this read-modify-write,
// never git operations.
func (s *Store) Update(fn func(*State) error) error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	st, err := s.Load()
	if err != nil {
		return err
	}
	if err := fn(st); err != nil {
		return err
	}
	return s.write(st)
}

// Replace overwrites the state under the lock (used by doctor).
func (s *Store) Replace(st *State) error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	return s.write(st)
}

func (s *Store) write(st *State) error {
	st.SchemaVersion = SchemaVersion
	st.Normalize()
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.Dir, "state.json.tmp*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), s.Path())
}

// Exclusive serializes whole operations that rewrite branches or worktrees:
// sync, pick, hoist and the rest hold it from start to finish. The state lock
// cannot do this — it is held only around each read-modify-write of
// state.json, so two syncs could otherwise rebase the same variant at once and
// each mistake the other's rebase for a conflict. When another process holds
// it, waiting is called with that holder's description before blocking.
func (s *Store) Exclusive(holder string, waiting func(other string)) (func(), error) {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(s.Dir, "op.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	busy, err := tryLockFile(f)
	if err == nil && busy {
		other, _ := os.ReadFile(path)
		waiting(strings.TrimSpace(string(other)))
		err = lockFile(f)
	}
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	// Advisory only, for the next waiter's message.
	f.Truncate(0)
	f.WriteAt([]byte(holder+"\n"), 0)
	return func() {
		f.Truncate(0)
		unlockFile(f)
		f.Close()
	}, nil
}

func (s *Store) lock() (func(), error) {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(s.lockPath(), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := lockFile(f); err != nil {
		f.Close()
		return nil, fmt.Errorf("locking %s: %w", s.lockPath(), err)
	}
	return func() {
		unlockFile(f)
		f.Close()
	}, nil
}
