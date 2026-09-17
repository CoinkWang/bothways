package gitx

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Version returns git's version triple.
func Version(ctx context.Context, dir string) ([3]int, error) {
	out, err := Run(ctx, dir, "version")
	if err != nil {
		return [3]int{}, err
	}
	// "git version 2.45.2" or "git version 2.39.3 (Apple Git-145)"
	fields := strings.Fields(out)
	if len(fields) < 3 {
		return [3]int{}, fmt.Errorf("unrecognized git version %q", out)
	}
	var v [3]int
	for i, part := range strings.SplitN(fields[2], ".", 4) {
		if i > 2 {
			break
		}
		n, err := strconv.Atoi(strings.TrimFunc(part, func(r rune) bool { return r < '0' || r > '9' }))
		if err != nil {
			break
		}
		v[i] = n
	}
	return v, nil
}

// AtLeast reports whether v >= min.
func AtLeast(v, min [3]int) bool {
	for i := range v {
		if v[i] != min[i] {
			return v[i] > min[i]
		}
	}
	return true
}

// CommonDir returns the absolute path of the shared git directory.
func CommonDir(ctx context.Context, dir string) (string, error) {
	return Run(ctx, dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
}

// GitPath resolves a path inside the worktree's private git dir (e.g. rebase-merge).
func GitPath(ctx context.Context, dir, name string) (string, error) {
	p, err := Run(ctx, dir, "rev-parse", "--git-path", name)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	return p, nil
}

// RevParse resolves rev to a full commit id.
func RevParse(ctx context.Context, dir, rev string) (string, error) {
	return Run(ctx, dir, "rev-parse", "--verify", "--end-of-options", rev+"^{commit}")
}

// BranchTip resolves refs/heads/<branch>; ok is false if the branch does not exist.
func BranchTip(ctx context.Context, dir, branch string) (tip string, ok bool, err error) {
	out, err := Run(ctx, dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	if err != nil {
		if ExitCode(err) == 1 {
			return "", false, nil
		}
		return "", false, err
	}
	return out, true, nil
}

// CurrentBranch returns the short branch name checked out in dir, or "" when detached.
func CurrentBranch(ctx context.Context, dir string) (string, error) {
	out, err := Run(ctx, dir, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		if ExitCode(err) == 1 {
			return "", nil
		}
		return "", err
	}
	return out, nil
}

// IsAncestor reports whether a is an ancestor of (or equal to) b.
func IsAncestor(ctx context.Context, dir, a, b string) (bool, error) {
	_, err := Run(ctx, dir, "merge-base", "--is-ancestor", a, b)
	if err == nil {
		return true, nil
	}
	if ExitCode(err) == 1 {
		return false, nil
	}
	return false, err
}

// MergeBase returns the best common ancestor of a and b.
func MergeBase(ctx context.Context, dir, a, b string) (string, error) {
	return Run(ctx, dir, "merge-base", a, b)
}

// Count returns the number of commits in from..to.
func Count(ctx context.Context, dir, from, to string) (int, error) {
	out, err := Run(ctx, dir, "rev-list", "--count", from+".."+to)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(out)
}

// RootCommit returns the oldest parentless commit reachable from rev.
func RootCommit(ctx context.Context, dir, rev string) (string, error) {
	out, err := Run(ctx, dir, "rev-list", "--max-parents=0", rev)
	if err != nil {
		return "", err
	}
	lines := strings.Split(out, "\n")
	return lines[len(lines)-1], nil
}

// UpdateRef moves ref to newOID only if it currently points at oldOID.
func UpdateRef(ctx context.Context, dir, msg, ref, newOID, oldOID string) error {
	_, err := Run(ctx, dir, "update-ref", "-m", msg, ref, newOID, oldOID)
	return err
}

// CreateBranch creates branch at start; it fails if the branch exists.
func CreateBranch(ctx context.Context, dir, branch, start string) error {
	_, err := Run(ctx, dir, "branch", "--no-track", "--end-of-options", branch, start)
	return err
}

// DeleteBranch force-deletes branch.
func DeleteBranch(ctx context.Context, dir, branch string) error {
	_, err := Run(ctx, dir, "branch", "-D", "--end-of-options", branch)
	return err
}

// RenameBranch renames old to new (config such as the description moves along).
func RenameBranch(ctx context.Context, dir, oldName, newName string) error {
	_, err := Run(ctx, dir, "branch", "-m", "--end-of-options", oldName, newName)
	return err
}

// ValidBranchName reports whether name is a legal branch name.
func ValidBranchName(ctx context.Context, dir, name string) bool {
	_, err := Run(ctx, dir, "check-ref-format", "--branch", name)
	return err == nil
}

// RefInfo is one entry of ListRefs.
type RefInfo struct {
	Name          string // full refname
	OID           string
	CommitterUnix int64
}

// ListRefs lists refs under prefix (e.g. "refs/heads/bothways/").
func ListRefs(ctx context.Context, dir, prefix string) ([]RefInfo, error) {
	out, err := Run(ctx, dir, "for-each-ref", "--format=%(refname)%00%(objectname)%00%(committerdate:unix)", prefix)
	if err != nil {
		return nil, err
	}
	var refs []RefInfo
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\x00")
		if len(parts) != 3 {
			return nil, fmt.Errorf("unexpected for-each-ref line %q", line)
		}
		ts, _ := strconv.ParseInt(parts[2], 10, 64)
		refs = append(refs, RefInfo{Name: parts[0], OID: parts[1], CommitterUnix: ts})
	}
	return refs, nil
}

// ConfigGet returns a config value; ok is false when unset.
func ConfigGet(ctx context.Context, dir, key string) (val string, ok bool, err error) {
	out, err := Run(ctx, dir, "config", "--get", key)
	if err != nil {
		if ExitCode(err) == 1 {
			return "", false, nil
		}
		return "", false, err
	}
	return out, true, nil
}

// ConfigSet writes a repository-local config value.
func ConfigSet(ctx context.Context, dir, key, val string) error {
	_, err := Run(ctx, dir, "config", "--local", key, val)
	return err
}

// ConfigUnset removes a repository-local config value (no error when unset).
func ConfigUnset(ctx context.Context, dir, key string) error {
	_, err := Run(ctx, dir, "config", "--local", "--unset-all", key)
	if ExitCode(err) == 5 {
		return nil
	}
	return err
}

// Dirty reports whether dir has uncommitted changes to tracked files, and
// optionally untracked (non-ignored) files.
func Dirty(ctx context.Context, dir string, includeUntracked bool) (bool, error) {
	untracked := "--untracked-files=no"
	if includeUntracked {
		untracked = "--untracked-files=normal"
	}
	out, err := Run(ctx, dir, "status", "--porcelain=v1", "-z", untracked)
	if err != nil {
		return false, err
	}
	return out != "", nil
}

// RebaseInProgress reports whether dir is stopped in the middle of a rebase.
func RebaseInProgress(ctx context.Context, dir string) (bool, error) {
	for _, name := range []string{"rebase-merge", "rebase-apply"} {
		p, err := GitPath(ctx, dir, name)
		if err != nil {
			return false, err
		}
		if _, err := os.Stat(p); err == nil {
			return true, nil
		}
	}
	return false, nil
}
