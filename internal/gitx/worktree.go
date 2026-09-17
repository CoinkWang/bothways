package gitx

import (
	"context"
	"strings"
)

// Worktree is one entry of `git worktree list --porcelain -z`.
type Worktree struct {
	Path     string
	HEAD     string
	Branch   string // short name, "" when detached or bare
	Bare     bool
	Detached bool
	Prunable bool
}

// Worktrees lists all worktrees of the repository containing dir. The first
// entry is the main worktree.
func Worktrees(ctx context.Context, dir string) ([]Worktree, error) {
	out, err := Run(ctx, dir, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, err
	}
	var list []Worktree
	var cur *Worktree
	for _, field := range strings.Split(out, "\x00") {
		if field == "" {
			if cur != nil {
				list = append(list, *cur)
				cur = nil
			}
			continue
		}
		key, val, _ := strings.Cut(field, " ")
		if key == "worktree" {
			cur = &Worktree{Path: val}
			continue
		}
		if cur == nil {
			continue
		}
		switch key {
		case "HEAD":
			cur.HEAD = val
		case "branch":
			cur.Branch = strings.TrimPrefix(val, "refs/heads/")
		case "bare":
			cur.Bare = true
		case "detached":
			cur.Detached = true
		case "prunable":
			cur.Prunable = true
		}
	}
	if cur != nil {
		list = append(list, *cur)
	}
	return list, nil
}

// WorktreeAdd checks out branch into path (or a detached commit when detach is set).
func WorktreeAdd(ctx context.Context, dir, path, rev string, detach bool) error {
	args := []string{"worktree", "add", "--quiet"}
	if detach {
		args = append(args, "--detach")
	}
	_, err := Run(ctx, dir, append(args, path, rev)...)
	return err
}

// WorktreeRemove removes a linked worktree; force discards local changes.
func WorktreeRemove(ctx context.Context, dir, path string, force bool) error {
	args := []string{"worktree", "remove"}
	if force {
		// Twice: also removes locked worktrees.
		args = append(args, "--force", "--force")
	}
	_, err := Run(ctx, dir, append(args, path)...)
	return err
}

// WorktreePrune drops administrative entries for worktrees whose directory is gone.
func WorktreePrune(ctx context.Context, dir string) error {
	_, err := Run(ctx, dir, "worktree", "prune")
	return err
}
