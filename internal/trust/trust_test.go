package trust

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStore(t *testing.T) {
	dir := t.TempDir()
	s := Store{Path: filepath.Join(dir, "state", "trust.json")}
	cfg := filepath.Join(dir, "repo", ".bothways.toml")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte("[worktree]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if sum, err := s.Lookup(cfg); err != nil || sum != "" {
		t.Fatalf("Lookup before any trust = %q, %v", sum, err)
	}
	if err := s.Trust(cfg, "aaa", time.Now()); err != nil {
		t.Fatal(err)
	}
	if sum, err := s.Lookup(cfg); err != nil || sum != "aaa" {
		t.Fatalf("Lookup = %q, %v; want aaa", sum, err)
	}
	if err := s.Trust(cfg, "bbb", time.Now()); err != nil {
		t.Fatal(err)
	}
	if sum, _ := s.Lookup(cfg); sum != "bbb" {
		t.Fatalf("Lookup after re-trust = %q; want bbb", sum)
	}

	// The same file reached through a symlink is the same entry.
	link := filepath.Join(dir, "link")
	if err := os.Symlink(filepath.Dir(cfg), link); err == nil {
		if sum, _ := s.Lookup(filepath.Join(link, ".bothways.toml")); sum != "bbb" {
			t.Errorf("Lookup through symlink = %q; want bbb", sum)
		}
	}

	if ok, err := s.Revoke(cfg); err != nil || !ok {
		t.Fatalf("Revoke = %v, %v", ok, err)
	}
	if ok, err := s.Revoke(cfg); err != nil || ok {
		t.Fatalf("second Revoke = %v, %v", ok, err)
	}
	if sum, _ := s.Lookup(cfg); sum != "" {
		t.Fatalf("Lookup after revoke = %q", sum)
	}
}

func TestStoreWithoutPath(t *testing.T) {
	var s Store
	if sum, err := s.Lookup("/x/.bothways.toml"); err != nil || sum != "" {
		t.Fatalf("Lookup = %q, %v", sum, err)
	}
	if err := s.Trust("/x/.bothways.toml", "aaa", time.Now()); err == nil {
		t.Fatal("Trust without a path succeeded")
	}
}
