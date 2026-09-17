// Package backend is the seam between bothways' model (a base branch, variant
// branches stacked on it, one checkout per variant) and the git operations
// that realise it. The cli layer decides what should happen and records it in
// state.json; a Backend makes it happen in the repository.
//
// # Where the cut is
//
// Reads of commit ids stay on gitx and run against the main worktree:
// rev-parse, rev-list --count, merge-base, for-each-ref, diff. What is cut here
// is everything with a multi-step procedure or a failure mode of its own:
//
//   - mutations of branches and commits,
//   - materialising, listing and removing checkouts,
//   - anything that looks at a checkout rather than at commit ids (dirty,
//     current branch, revisions typed relative to it),
//   - conflict detection, and the words a message uses to tell the user how
//     to finish a conflict.
//
// A conflict is a Result a rewrite returns, never a state to query
// afterwards, so the cli never has to reason about half-finished rebases.
//
// Commit ids cross the interface as full hex strings; branch names are short
// names (bothways/<decision>/<variant>), never refs/heads/ paths.
package backend

import "context"

// Backend performs the repository side of every bothways command.
type Backend interface {
	// Init prepares the repository once (git: turn rerere on unless the user
	// configured it).
	Init(ctx context.Context) error

	// CreateBranch creates branch at commit and fails if it exists.
	CreateBranch(ctx context.Context, branch, at string) error
	// MoveBranch moves branch from one commit to another only if it still
	// points at from. Checkouts of the branch are not touched: see FollowBranch.
	MoveBranch(ctx context.Context, branch, from, to, reason string) error
	// RenameBranch renames a branch together with its description.
	RenameBranch(ctx context.Context, from, to string) error
	// DeleteBranch deletes a branch whether or not it is merged anywhere.
	DeleteBranch(ctx context.Context, branch string) error
	// Description returns what SetDescription stored, "" when nothing was.
	Description(ctx context.Context, branch string) (string, error)
	// SetDescription attaches free text to a branch, so that `doctor` can
	// rebuild decisions from branches alone.
	SetDescription(ctx context.Context, branch, text string) error

	// Workspaces lists checkouts; the first is the main one.
	Workspaces(ctx context.Context) ([]Workspace, error)
	// AddWorkspace checks branch out at path, so that commits made there move
	// the branch. The parent directory must exist.
	AddWorkspace(ctx context.Context, path, branch string) error
	// AddDetachedWorkspace checks out a commit at path without any branch.
	AddDetachedWorkspace(ctx context.Context, path, commit string) error
	// RemoveWorkspace removes a checkout, discarding whatever is in it.
	RemoveWorkspace(ctx context.Context, path string) error
	// PruneWorkspaces forgets checkouts whose directory is gone.
	PruneWorkspaces(ctx context.Context) error
	// FollowBranch updates the files of the checkout at dir after its branch
	// moved from one commit to another with MoveBranch.
	FollowBranch(ctx context.Context, dir, from, to string) error

	// CurrentBranch is the branch checked out at dir, "" when there is none.
	CurrentBranch(ctx context.Context, dir string) (string, error)
	// Dirty reports uncommitted changes at dir, optionally counting untracked
	// (non-ignored) files.
	Dirty(ctx context.Context, dir string, includeUntracked bool) (bool, error)
	// Head is the commit checked out at dir.
	Head(ctx context.Context, dir string) (string, error)
	// ResolveCommit resolves a revision the user typed, relative to dir.
	ResolveCommit(ctx context.Context, dir, rev string) (string, error)
	// ResolveRange resolves a user-typed range to its commits, oldest first.
	ResolveRange(ctx context.Context, dir, spec string) ([]string, error)

	// SaveChanges records the uncommitted changes at dir without touching
	// them. The zero Saved means there was nothing to record.
	SaveChanges(ctx context.Context, dir, message string) (Saved, error)
	// ApplySaved applies saved changes at dir. When that fails they are kept
	// where Vocab().SavedList finds them.
	ApplySaved(ctx context.Context, saved Saved, dir string) error
	// DiscardChanges throws away uncommitted changes to tracked files at dir.
	DiscardChanges(ctx context.Context, dir string) error

	// Restack rewrites a branch so that Upstream and everything below it is
	// replaced by Onto.
	Restack(ctx context.Context, r Restack) (Result, error)
	// Replay copies commits, oldest first, onto a commit and returns the copy
	// of the newest. No branch moves. A conflict is a *ConflictError; a failure
	// to set up a place to work is a *ScratchError.
	Replay(ctx context.Context, onto string, commits []string) (string, error)
	// Excise takes contiguous commits out of a branch after their copies
	// landed on base.
	Excise(ctx context.Context, e Excise) (Result, error)
	// Merge merges a commit into the detached checkout at dir. A conflict is
	// a *ConflictError listing the files and leaves dir as it was.
	Merge(ctx context.Context, dir, commit, message string) error
	// InConflict reports whether the checkout at dir holds a conflict the
	// user still has to resolve.
	InConflict(ctx context.Context, dir string) (bool, error)

	// Vocab is how messages name this backend's conflict and saved changes.
	Vocab() Vocab
	// FollowCommand is the command a user runs to do FollowBranch by hand.
	FollowCommand(dir, from, to string) string
}

// Workspace is one checkout.
type Workspace struct {
	Path     string
	Branch   string // "" when detached
	Prunable bool   // registered, but the directory is gone
}

// Saved identifies changes recorded by SaveChanges.
type Saved struct {
	Dir string // where they were recorded from
	ID  string // "" when there was nothing to record
}

// Restack describes one Restack.
type Restack struct {
	Branch   string
	Tip      string // current tip of Branch
	Upstream string // replaced, together with its ancestors, by Onto
	Onto     string
	// Workspace is the checkout of Branch. With one, uncommitted changes are
	// kept and a conflict is left there for the user. Without, the work
	// happens out of sight and a conflict changes nothing.
	Workspace string
	Label     string // "bothways <command> <ref>"; names whatever trace the operation leaves
}

// Excise describes one Excise.
type Excise struct {
	Branch  string
	Tip     string   // current tip of Branch
	Commits []string // oldest first, contiguous
	Base    string   // tip that now carries copies of Commits
	// Workspace and Label are as for Restack.
	Workspace string
	Label     string
}

// Result is how a Restack or Excise ended.
type Result struct {
	// Conflict: with a workspace, the conflict waits there for the user; the
	// branch is not where it is headed until it is resolved. Without one,
	// nothing changed.
	Conflict bool
	// Saved: uncommitted changes of the workspace were set aside for the
	// conflict and are not back yet.
	Saved bool
	// NotRestored: the branch was rewritten, but putting the uncommitted
	// changes back conflicted; they are still set aside.
	NotRestored bool
}

// Vocab holds the backend-specific words of user-facing messages.
type Vocab struct {
	Stopped   string // a conflict left in a workspace: "rebase"
	Continue  string // how to finish one: "`git rebase --continue`"
	Saving    string // setting uncommitted changes aside: "stashing"
	Saved     string // where they went: "uncommitted changes were stashed: `git stash pop` afterwards"
	DropSaved string // discarding the set-aside copy after a manual restore: "`git stash drop`"
	SavedList string // where ApplySaved keeps changes it could not apply: "`git stash list`"
}

// ConflictError is returned when commits do not combine cleanly and nothing
// was left behind for the user.
type ConflictError struct {
	Files []string // conflicting paths, when known
	Err   error
}

func (e *ConflictError) Error() string { return e.Err.Error() }
func (e *ConflictError) Unwrap() error { return e.Err }

// ScratchError is returned when the throwaway checkout an operation works in
// cannot be created.
type ScratchError struct {
	What string // "directory" or "worktree"
	Err  error
}

func (e *ScratchError) Error() string { return "creating a temporary " + e.What + ": " + e.Err.Error() }
func (e *ScratchError) Unwrap() error { return e.Err }

// SaveError is returned by Restack and Excise when uncommitted changes in
// the workspace cannot be set aside; nothing was rewritten.
type SaveError struct {
	Err error
}

func (e *SaveError) Error() string { return e.Err.Error() }
func (e *SaveError) Unwrap() error { return e.Err }
