// Package git implements backend.Backend by shelling out to git through gitx.
// Repository-wide commands run with -C on the main worktree; commands about one
// checkout run with -C on that checkout.
package git

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/CoinkWang/bothways/internal/backend"
	"github.com/CoinkWang/bothways/internal/gitx"
)

// Backend is the git backend.
type Backend struct {
	root string // main worktree
}

var _ backend.Backend = (*Backend)(nil)

// New returns the backend for the repository whose main worktree is root.
func New(root string) *Backend { return &Backend{root: root} }

// noEditor keeps rebase, cherry-pick and merge from opening an editor.
var noEditor = []string{"GIT_EDITOR=:"}

func (b *Backend) Init(ctx context.Context) error {
	if _, set, err := gitx.ConfigGet(ctx, b.root, "rerere.enabled"); err != nil {
		return err
	} else if !set {
		return gitx.ConfigSet(ctx, b.root, "rerere.enabled", "true")
	}
	return nil
}

func (b *Backend) CreateBranch(ctx context.Context, branch, at string) error {
	return gitx.CreateBranch(ctx, b.root, branch, at)
}

func (b *Backend) MoveBranch(ctx context.Context, branch, from, to, reason string) error {
	return gitx.UpdateRef(ctx, b.root, reason, "refs/heads/"+branch, to, from)
}

func (b *Backend) RenameBranch(ctx context.Context, from, to string) error {
	return gitx.RenameBranch(ctx, b.root, from, to)
}

func (b *Backend) DeleteBranch(ctx context.Context, branch string) error {
	return gitx.DeleteBranch(ctx, b.root, branch)
}

// Descriptions live in branch.<name>.description, which `git branch -m`
// carries along and `git branch --edit-description` shows.
func (b *Backend) Description(ctx context.Context, branch string) (string, error) {
	desc, _, err := gitx.ConfigGet(ctx, b.root, "branch."+branch+".description")
	return desc, err
}

func (b *Backend) SetDescription(ctx context.Context, branch, text string) error {
	return gitx.ConfigSet(ctx, b.root, "branch."+branch+".description", text)
}

func (b *Backend) Workspaces(ctx context.Context) ([]backend.Workspace, error) {
	wts, err := gitx.Worktrees(ctx, b.root)
	if err != nil {
		return nil, err
	}
	out := make([]backend.Workspace, len(wts))
	for i, wt := range wts {
		out[i] = backend.Workspace{Path: wt.Path, Branch: wt.Branch, Prunable: wt.Prunable}
	}
	return out, nil
}

func (b *Backend) AddWorkspace(ctx context.Context, path, branch string) error {
	return gitx.WorktreeAdd(ctx, b.root, path, branch, false)
}

func (b *Backend) AddDetachedWorkspace(ctx context.Context, path, commit string) error {
	return gitx.WorktreeAdd(ctx, b.root, path, commit, true)
}

func (b *Backend) RemoveWorkspace(ctx context.Context, path string) error {
	return gitx.WorktreeRemove(ctx, b.root, path, true)
}

func (b *Backend) PruneWorkspaces(ctx context.Context) error {
	return gitx.WorktreePrune(ctx, b.root)
}

// FollowBranch uses a two-tree read-tree rather than merge --ff-only: the
// branch has already moved, so HEAD no longer describes what is on disk.
func (b *Backend) FollowBranch(ctx context.Context, dir, from, to string) error {
	gitx.Run(ctx, dir, "update-index", "-q", "--refresh")
	_, err := gitx.Run(ctx, dir, "read-tree", "-m", "-u", from, to)
	return err
}

func (b *Backend) FollowCommand(dir, from, to string) string {
	return fmt.Sprintf("git -C %s read-tree -m -u %s %s", dir, from, to)
}

func (b *Backend) CurrentBranch(ctx context.Context, dir string) (string, error) {
	return gitx.CurrentBranch(ctx, dir)
}

func (b *Backend) Dirty(ctx context.Context, dir string, includeUntracked bool) (bool, error) {
	return gitx.Dirty(ctx, dir, includeUntracked)
}

func (b *Backend) Head(ctx context.Context, dir string) (string, error) {
	return gitx.RevParse(ctx, dir, "HEAD")
}

func (b *Backend) ResolveCommit(ctx context.Context, dir, rev string) (string, error) {
	return gitx.RevParse(ctx, dir, rev)
}

func (b *Backend) ResolveRange(ctx context.Context, dir, spec string) ([]string, error) {
	out, err := gitx.Run(ctx, dir, "rev-list", "--reverse", "--end-of-options", spec)
	if err != nil {
		return nil, err
	}
	var commits []string
	for _, line := range strings.Split(out, "\n") {
		if line != "" {
			commits = append(commits, line)
		}
	}
	return commits, nil
}

// SaveChanges uses stash create, which writes the stash commit without
// touching the worktree or the stash list.
func (b *Backend) SaveChanges(ctx context.Context, dir, message string) (backend.Saved, error) {
	id, err := gitx.Run(ctx, dir, "stash", "create", message)
	if err != nil {
		return backend.Saved{}, err
	}
	return backend.Saved{Dir: dir, ID: id}, nil
}

func (b *Backend) ApplySaved(ctx context.Context, saved backend.Saved, dir string) error {
	_, err := gitx.Run(ctx, dir, "stash", "apply", "--quiet", saved.ID)
	if err != nil {
		gitx.Run(ctx, saved.Dir, "stash", "store", "-m", "bothways carry (failed apply)", saved.ID)
	}
	return err
}

func (b *Backend) DiscardChanges(ctx context.Context, dir string) error {
	_, err := gitx.Run(ctx, dir, "reset", "--hard", "--quiet")
	return err
}

func (b *Backend) InConflict(ctx context.Context, dir string) (bool, error) {
	return gitx.RebaseInProgress(ctx, dir)
}

func (b *Backend) Vocab() backend.Vocab {
	return backend.Vocab{
		Stopped:   "rebase",
		Continue:  "`git rebase --continue`",
		Saving:    "stashing",
		Saved:     "uncommitted changes were stashed: `git stash pop` afterwards",
		DropSaved: "`git stash drop`",
		SavedList: "`git stash list`",
	}
}

// Restack runs `rebase --onto` in the variant's own worktree, because that is
// where a conflict has to be resolved, stashing uncommitted changes around it.
// Without a worktree it rebases in a throwaway one and aborts on
// conflict.
func (b *Backend) Restack(ctx context.Context, r backend.Restack) (backend.Result, error) {
	dir := r.Workspace
	if dir == "" {
		tmp, cleanup, err := b.scratch(ctx, "bothways-"+scratchPrefix(r.Label)+"-", r.Tip)
		if err != nil {
			return backend.Result{}, err
		}
		defer cleanup()
		dir = tmp
	}

	stashed := false
	if r.Workspace != "" {
		dirty, err := gitx.Dirty(ctx, dir, false)
		if err != nil {
			return backend.Result{}, err
		}
		if dirty {
			if _, err := gitx.Run(ctx, dir, "stash", "push", "--quiet", "-m", r.Label); err != nil {
				return backend.Result{}, &backend.SaveError{Err: err}
			}
			stashed = true
		}
	}

	_, rebaseErr := gitx.Exec(ctx, gitx.Cmd{
		Dir:  dir,
		Args: []string{"rebase", "--update-refs", "--onto", r.Onto, r.Upstream, r.Branch},
		Env:  noEditor,
	})
	if rebaseErr != nil {
		busy, _ := gitx.RebaseInProgress(ctx, dir)
		if !busy {
			if stashed {
				gitx.Run(ctx, dir, "stash", "pop", "--quiet")
			}
			return backend.Result{}, rebaseErr
		}
		if r.Workspace == "" {
			gitx.Run(ctx, dir, "rebase", "--abort")
		}
		return backend.Result{Conflict: true, Saved: stashed}, nil
	}
	if stashed {
		if _, err := gitx.Run(ctx, dir, "stash", "pop", "--quiet"); err != nil {
			return backend.Result{NotRestored: true}, nil
		}
	}
	return backend.Result{}, nil
}

// scratchPrefix names the temporary directory after the command in a label of
// the form "bothways <command> <ref>": bothways-sync-*, bothways-hoist-*.
func scratchPrefix(label string) string {
	word, _, _ := strings.Cut(strings.TrimPrefix(label, "bothways "), " ")
	if word == "" {
		return "tmp"
	}
	return word
}

// scratch creates a detached worktree at commit that cleanup removes again.
func (b *Backend) scratch(ctx context.Context, prefix, commit string) (string, func(), error) {
	tmp, err := os.MkdirTemp("", prefix)
	if err != nil {
		return "", nil, &backend.ScratchError{What: "directory", Err: err}
	}
	cleanup := func() {
		gitx.WorktreeRemove(ctx, b.root, tmp, true)
		os.RemoveAll(tmp)
	}
	if err := gitx.WorktreeAdd(ctx, b.root, tmp, commit, true); err != nil {
		cleanup()
		return "", nil, &backend.ScratchError{What: "worktree", Err: err}
	}
	return tmp, cleanup, nil
}

// Replay cherry-picks in a throwaway detached worktree, so that neither the
// base worktree nor any variant worktree index is touched.
func (b *Backend) Replay(ctx context.Context, onto string, commits []string) (string, error) {
	tmp, cleanup, err := b.scratch(ctx, "bothways-hoist-", onto)
	if err != nil {
		return "", err
	}
	defer cleanup()
	_, err = gitx.Exec(ctx, gitx.Cmd{
		Dir:  tmp,
		Args: append([]string{"cherry-pick"}, commits...),
		Env:  noEditor,
	})
	if err != nil {
		return "", &backend.ConflictError{Err: err}
	}
	return gitx.RevParse(ctx, tmp, "HEAD")
}

// Excise is one rebase that drops the newest excised commit and everything
// below it. See ExciseOnto for what replaces them.
func (b *Backend) Excise(ctx context.Context, e backend.Excise) (backend.Result, error) {
	if len(e.Commits) == 0 {
		return backend.Result{}, fmt.Errorf("excise: no commits")
	}
	onto, err := ExciseOnto(ctx, b.root, e.Commits[0], e.Base)
	if err != nil {
		return backend.Result{}, err
	}
	return b.Restack(ctx, backend.Restack{
		Branch:    e.Branch,
		Tip:       e.Tip,
		Upstream:  e.Commits[len(e.Commits)-1],
		Onto:      onto,
		Workspace: e.Workspace,
		Label:     e.Label,
	})
}

// ExciseOnto chooses what replaces the excised range. `rebase --onto X
// <newest>` discards <newest> and all of its ancestors, so X must supply
// whatever of that the branch still needs. The base only does when the
// parent of the oldest excised commit is already on it, that is when nothing
// below the range is the variant's own; then the variant lands on the base in
// one go. Otherwise the parent itself is the answer: exactly the range goes,
// the variant's earlier commits stay, and the following sync restacks them.
func ExciseOnto(ctx context.Context, dir, oldest, base string) (string, error) {
	pre, err := gitx.RevParse(ctx, dir, oldest+"^")
	if err != nil {
		return "", err
	}
	rooted, err := gitx.IsAncestor(ctx, dir, pre, base)
	if err != nil {
		return "", err
	}
	if rooted {
		return base, nil
	}
	return pre, nil
}

// Merge leaves the worktree as it was on failure, so that it can be removed
// or retried; the conflicting paths are read before the merge is aborted.
func (b *Backend) Merge(ctx context.Context, dir, commit, message string) error {
	_, err := gitx.Exec(ctx, gitx.Cmd{
		Dir:  dir,
		Args: []string{"merge", "--no-edit", "-m", message, commit},
		Env:  noEditor,
	})
	if err == nil {
		return nil
	}
	files := unmergedFiles(ctx, dir)
	gitx.Run(ctx, dir, "merge", "--abort")
	if len(files) == 0 {
		return err
	}
	return &backend.ConflictError{Files: files, Err: err}
}

func unmergedFiles(ctx context.Context, dir string) []string {
	out, err := gitx.Run(ctx, dir, "diff", "--name-only", "--diff-filter=U")
	if err != nil || out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}
