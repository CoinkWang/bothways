// Package worktree places variant worktrees outside the repository and makes
// a fresh checkout runnable: gitignored path strategies and a one-shot setup
// command. Its failures degrade (warnings), they never break variant logic.
package worktree

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/CoinkWang/bothways/internal/config"
	"github.com/CoinkWang/bothways/internal/gitx"
)

// RepoID is the first 12 hex digits of the repository's root commit.
func RepoID(ctx context.Context, repoDir, rev string) (string, error) {
	root, err := gitx.RootCommit(ctx, repoDir, rev)
	if err != nil {
		return "", err
	}
	if len(root) < 12 {
		return "", fmt.Errorf("unexpected root commit %q", root)
	}
	return root[:12], nil
}

// CacheHome returns $XDG_CACHE_HOME or ~/.cache.
func CacheHome() (string, error) {
	if dir := os.Getenv("XDG_CACHE_HOME"); dir != "" {
		return filepath.Abs(dir)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	// HOME can be relative (some CI runners, sandboxes); worktree paths end up
	// in status --json, which promises absolute paths.
	return filepath.Abs(filepath.Join(home, ".cache"))
}

// DefaultRoot returns $XDG_CACHE_HOME/bothways/<repo-id>.
func DefaultRoot(repoID string) (string, error) {
	cache, err := CacheHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(cache, "bothways", repoID), nil
}

// Path returns <root>/<decision>/<variant>.
func Path(root, decision, variant string) string {
	return filepath.Join(root, decision, variant)
}

// Spec describes a worktree to materialize.
type Spec struct {
	RepoDir  string // any directory of the repository, used for -C
	SeedDir  string // worktree gitignored paths are copied from ("" to skip)
	Path     string
	Branch   string
	Decision string
	Variant  string
}

// Env returns the BOTHWAYS_* variables for commands run in the worktree.
func (s Spec) Env() []string {
	return []string{
		"BOTHWAYS_NAME=" + s.Decision + "-" + s.Variant,
		"BOTHWAYS_DECISION=" + s.Decision,
		"BOTHWAYS_VARIANT=" + s.Variant,
		"BOTHWAYS_WORKTREE=" + s.Path,
	}
}

// Add checks the branch out at s.Path.
func Add(ctx context.Context, s Spec) error {
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
		return err
	}
	return gitx.WorktreeAdd(ctx, s.RepoDir, s.Path, s.Branch, false)
}

// ApplyPaths carries gitignored paths from s.SeedDir into the new worktree
// according to rules. Problems are reported through warn.
func ApplyPaths(ctx context.Context, s Spec, rules []config.PathRule, warn func(string)) {
	if s.SeedDir == "" {
		return
	}
	for _, r := range rules {
		if r.Strategy == config.StrategyFresh {
			continue
		}
		rel := filepath.Clean(strings.TrimSuffix(r.Path, "/"))
		src := filepath.Join(s.SeedDir, rel)
		dst := filepath.Join(s.Path, rel)
		if _, err := os.Lstat(src); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if _, err := gitx.Run(ctx, s.SeedDir, "check-ignore", "-q", "--", rel); err != nil {
			if gitx.ExitCode(err) == 1 {
				warn(fmt.Sprintf("[worktree.paths] %q is not gitignored; skipped", r.Path))
			} else {
				warn(fmt.Sprintf("[worktree.paths] %q: %v", r.Path, err))
			}
			continue
		}
		if _, err := os.Lstat(dst); err == nil {
			continue
		}
		if err := carry(ctx, r, src, dst, warn); err != nil {
			warn(fmt.Sprintf("[worktree.paths] carrying %q: %v", r.Path, err))
		}
	}
}

// carry materializes src at dst. A strategy is a performance hint, never a
// correctness requirement: a filesystem that will not clone and a platform
// that will not symlink both degrade to a plain copy.
func carry(ctx context.Context, r config.PathRule, src, dst string, warn func(string)) error {
	switch r.Strategy {
	case config.StrategyClone:
		err := cloneTree(ctx, src, dst)
		if err == nil {
			return nil
		}
		os.RemoveAll(dst) // a half-written clone would poison the copy
		warn(fmt.Sprintf("[worktree.paths] %q: copy-on-write unavailable (%v); copying instead", r.Path, err))
	case config.StrategyLink:
		err := linkTree(src, dst)
		if err == nil {
			return nil
		}
		warn(fmt.Sprintf("[worktree.paths] %q: symlink failed (%v); copying instead", r.Path, err))
	}
	return copyTree(src, dst)
}

// cloneArgv is the platform's copy-on-write copy, empty where there is none.
// Shelling out keeps clonefile and reflink out of the dependency list; tests
// replace it to exercise the fallback.
var cloneArgv = func(src, dst string) []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{"cp", "-Rc", src, dst}
	case "linux":
		return []string{"cp", "-a", "--reflink=auto", src, dst}
	}
	return nil
}

// cloneTree asks the filesystem for a copy-on-write copy of src at dst.
func cloneTree(ctx context.Context, src, dst string) error {
	argv := cloneArgv(src, dst)
	if len(argv) == 0 {
		return fmt.Errorf("no copy-on-write copy on %s", runtime.GOOS)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput()
	if err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("%s: %v: %s", argv[0], err, msg)
		}
		return fmt.Errorf("%s: %v", argv[0], err)
	}
	return nil
}

// linkTree points dst at the shared path. Windows grants symlink creation only
// to privileged or developer-mode processes, so the error path matters there.
func linkTree(src, dst string) error {
	target, err := filepath.Abs(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.Symlink(target, dst)
}

// RunSetup runs the setup command in the worktree through the shell.
func RunSetup(ctx context.Context, s Spec, command string, stdout, stderr io.Writer) error {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/C", command)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", command)
	}
	cmd.Dir = s.Path
	cmd.Env = append(os.Environ(), s.Env()...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

// Fingerprint hashes the named files in dir (missing files count as empty
// markers). It returns "" when files is empty.
func Fingerprint(dir string, files []string) (string, error) {
	if len(files) == 0 {
		return "", nil
	}
	h := sha256.New()
	for _, name := range files {
		fmt.Fprintf(h, "%s\x00", name)
		f, err := os.Open(filepath.Join(dir, name))
		if errors.Is(err, os.ErrNotExist) {
			h.Write([]byte("missing\x00"))
			continue
		}
		if err != nil {
			return "", err
		}
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return "", err
		}
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// copyTree copies a file, symlink or directory tree, preserving modes.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		case info.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			return os.Symlink(link, target)
		case info.Mode().IsRegular():
			return copyFile(path, target, info.Mode().Perm())
		default:
			return nil // sockets, devices: skip
		}
	})
}

func copyFile(src, dst string, perm fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// SamePath reports whether two paths name the same location, resolving symlinks
// (e.g. /var vs /private/var on macOS).
func SamePath(a, b string) bool {
	if a == "" || b == "" {
		return a == b
	}
	return Canonical(a) == Canonical(b)
}

// Canonical resolves symlinks where possible and cleans the path.
func Canonical(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	// The path may not exist (anymore); resolve its closest existing ancestor.
	p = filepath.Clean(p)
	var rest []string
	for dir := p; ; {
		parent, base := filepath.Dir(dir), filepath.Base(dir)
		if parent == dir {
			return p
		}
		rest = append([]string{base}, rest...)
		if real, err := filepath.EvalSymlinks(parent); err == nil {
			return filepath.Join(append([]string{real}, rest...)...)
		}
		dir = parent
	}
}

// Within reports whether p is inside (or equal to) root.
func Within(root, p string) bool {
	rel, err := filepath.Rel(Canonical(root), Canonical(p))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
