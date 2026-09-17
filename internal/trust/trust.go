// Package trust records which repository config files the user has reviewed.
// A .bothways.toml can make bothways run shell commands, so none of them runs
// until the user trusts the file's exact content. Decisions live in the user's
// state directory, never in a repository: a repository cannot trust itself.
package trust

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// SchemaVersion is the version of the trust file.
const SchemaVersion = 1

// DefaultPath is $XDG_STATE_HOME/bothways/trust.json, falling back to
// ~/.local/state. It is "" when neither can be determined.
func DefaultPath() string {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "bothways", "trust.json")
}

// Store reads and writes one trust file.
type Store struct {
	Path string
}

type entry struct {
	SHA256    string    `json:"sha256"`
	TrustedAt time.Time `json:"trusted_at"`
}

type document struct {
	SchemaVersion int              `json:"schema_version"`
	Files         map[string]entry `json:"files"`
}

// Lookup returns the digest last trusted for the config file at path, "" when
// it was never trusted.
func (s Store) Lookup(path string) (string, error) {
	doc, err := s.read()
	if err != nil {
		return "", err
	}
	return doc.Files[key(path)].SHA256, nil
}

// Trust records sum as the reviewed content of the config file at path,
// replacing whatever was trusted for it before.
func (s Store) Trust(path, sum string, at time.Time) error {
	doc, err := s.read()
	if err != nil {
		return err
	}
	doc.Files[key(path)] = entry{SHA256: sum, TrustedAt: at.UTC()}
	return s.write(doc)
}

// Revoke forgets the config file at path and reports whether it was trusted.
func (s Store) Revoke(path string) (bool, error) {
	doc, err := s.read()
	if err != nil {
		return false, err
	}
	k := key(path)
	if _, ok := doc.Files[k]; !ok {
		return false, nil
	}
	delete(doc.Files, k)
	return true, s.write(doc)
}

func (s Store) read() (*document, error) {
	doc := &document{SchemaVersion: SchemaVersion, Files: map[string]entry{}}
	if s.Path == "" {
		return doc, nil
	}
	data, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return doc, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, doc); err != nil {
		return nil, fmt.Errorf("%s: %w", s.Path, err)
	}
	if doc.Files == nil {
		doc.Files = map[string]entry{}
	}
	return doc, nil
}

// write replaces the file atomically. Two processes trusting at the same moment
// can lose one of the entries; that only ever leaves a file untrusted.
func (s Store) write(doc *document) error {
	if s.Path == "" {
		return errors.New("no state directory: set XDG_STATE_HOME or HOME")
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.Path), ".trust-*.json")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(append(data, '\n'))
	cerr := tmp.Close()
	if werr == nil {
		werr = cerr
	}
	if werr != nil {
		os.Remove(tmp.Name())
		return werr
	}
	if err := os.Rename(tmp.Name(), s.Path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// key identifies a config file by its absolute path with symlinks resolved, so
// the same file reached two ways is trusted once.
func key(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	return filepath.Clean(path)
}
