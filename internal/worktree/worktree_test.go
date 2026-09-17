package worktree

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/CoinkWang/bothways/internal/config"
)

func TestCopyTree(t *testing.T) {
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "a", "b"), 0o755)
	os.WriteFile(filepath.Join(src, "a", "b", "f.txt"), []byte("hi"), 0o600)
	os.WriteFile(filepath.Join(src, "a", "run.sh"), []byte("#!/bin/sh"), 0o755)
	if err := os.Symlink("b/f.txt", filepath.Join(src, "a", "link")); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "out")
	if err := copyTree(filepath.Join(src, "a"), dst); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(dst, "b", "f.txt")); string(data) != "hi" {
		t.Errorf("content = %q", data)
	}
	if info, _ := os.Stat(filepath.Join(dst, "run.sh")); info.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v", info.Mode())
	}
	if link, _ := os.Readlink(filepath.Join(dst, "link")); link != "b/f.txt" {
		t.Errorf("link = %q", link)
	}
}

func TestFingerprint(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	for _, dir := range []string{a, b} {
		os.WriteFile(filepath.Join(dir, "lock"), []byte("v1"), 0o644)
	}
	fa, _ := Fingerprint(a, []string{"lock", "absent"})
	fb, _ := Fingerprint(b, []string{"lock", "absent"})
	if fa == "" || fa != fb {
		t.Errorf("fingerprints differ for equal files: %s %s", fa, fb)
	}
	os.WriteFile(filepath.Join(b, "lock"), []byte("v2"), 0o644)
	if fb2, _ := Fingerprint(b, []string{"lock", "absent"}); fb2 == fa {
		t.Error("fingerprint unchanged after edit")
	}
	if f, _ := Fingerprint(a, nil); f != "" {
		t.Errorf("empty list fingerprint = %q", f)
	}
}

func TestCarryCloneFallsBackToCopy(t *testing.T) {
	restore := cloneArgv
	cloneArgv = func(src, dst string) []string { return []string{"false"} } // a filesystem that refuses reflinks
	t.Cleanup(func() { cloneArgv = restore })

	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "pkg"), []byte("dep"), 0o644)
	dst := filepath.Join(t.TempDir(), "node_modules")
	var warned []string
	rule := config.PathRule{Path: "node_modules", Strategy: config.StrategyClone}
	if err := carry(context.Background(), rule, src, dst, func(m string) { warned = append(warned, m) }); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(dst, "pkg")); string(data) != "dep" {
		t.Errorf("content = %q, want the plain copy", data)
	}
	if len(warned) != 1 || !strings.Contains(warned[0], "copy-on-write unavailable") {
		t.Errorf("warnings = %v, want one degrade warning", warned)
	}
}

func TestCarryClone(t *testing.T) {
	if len(cloneArgv("a", "b")) == 0 {
		t.Skipf("no copy-on-write copy on %s", runtime.GOOS)
	}
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "pkg"), []byte("dep"), 0o644)
	dst := filepath.Join(t.TempDir(), "node_modules")
	rule := config.PathRule{Path: "node_modules", Strategy: config.StrategyClone}
	if err := carry(context.Background(), rule, src, dst, func(m string) { t.Logf("degraded: %s", m) }); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(dst, "pkg")); string(data) != "dep" {
		t.Errorf("content = %q", data)
	}
}

func TestCarryLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks needs privileges on Windows")
	}
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "weights.bin"), []byte("big"), 0o644)
	dst := filepath.Join(t.TempDir(), "data")
	rule := config.PathRule{Path: "data/", Strategy: config.StrategyLink}
	if err := carry(context.Background(), rule, src, dst, func(m string) { t.Errorf("warned: %s", m) }); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&fs.ModeSymlink == 0 {
		t.Fatalf("%s is not a symlink (mode %v)", dst, info.Mode())
	}
	if target, _ := os.Readlink(dst); !SamePath(target, src) {
		t.Errorf("link target = %q, want %q", target, src)
	}
	if data, _ := os.ReadFile(filepath.Join(dst, "weights.bin")); string(data) != "big" {
		t.Errorf("content through the link = %q", data)
	}
}

func TestWithin(t *testing.T) {
	root := t.TempDir()
	if !Within(root, filepath.Join(root, "x", "y")) || Within(root, filepath.Dir(root)) || Within(root, root+"-other") {
		t.Error("Within misclassified paths")
	}
}

func TestCacheHomeIsAbsoluteWithRelativeHome(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("HOME", "relative-home")
	t.Setenv("USERPROFILE", "relative-home")
	got, err := CacheHome()
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("CacheHome() = %q, want an absolute path", got)
	}
}
