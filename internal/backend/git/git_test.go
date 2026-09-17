package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/CoinkWang/bothways/internal/backend"
	"github.com/CoinkWang/bothways/internal/gitx"
)

type repo struct {
	t    *testing.T
	ctx  context.Context
	dir  string
	tmp  string
	back *Backend
}

func newRepo(t *testing.T) *repo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	tmp := t.TempDir()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(tmp, "gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	r := &repo{t: t, ctx: context.Background(), dir: filepath.Join(tmp, "repo"), tmp: tmp}
	r.git(tmp, "init", "-q", "-b", "main", r.dir)
	r.git(r.dir, "config", "user.name", "T")
	r.git(r.dir, "config", "user.email", "t@example.com")
	r.back = New(r.dir)
	r.commit(r.dir, "page.txt", "brand list", "base: brand list")
	return r
}

func (r *repo) git(dir string, args ...string) string {
	r.t.Helper()
	out, err := gitx.Run(r.ctx, dir, args...)
	if err != nil {
		r.t.Fatal(err)
	}
	return out
}

func (r *repo) commit(dir, file, content, msg string) string {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content+"\n"), 0o644); err != nil {
		r.t.Fatal(err)
	}
	r.git(dir, "add", file)
	r.git(dir, "commit", "-q", "-m", msg)
	return r.git(dir, "rev-parse", "HEAD")
}

func (r *repo) tip(branch string) string {
	r.t.Helper()
	tip, ok, err := gitx.BranchTip(r.ctx, r.dir, branch)
	if err != nil || !ok {
		r.t.Fatalf("branch %s: %v %v", branch, ok, err)
	}
	return tip
}

// subjects lists the commit subjects of from..to, newest first.
func (r *repo) subjects(from, to string) []string {
	r.t.Helper()
	out := r.git(r.dir, "log", "--format=%s", from+".."+to)
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// variant creates branch v at main with one commit per subject, built in a
// worktree that is removed again, and returns the commit ids.
func (r *repo) variant(v string, subjects ...string) []string {
	r.t.Helper()
	wt := filepath.Join(r.tmp, "build-"+v)
	r.git(r.dir, "branch", v, "main")
	r.git(r.dir, "worktree", "add", "-q", wt, v)
	var ids []string
	for i, s := range subjects {
		ids = append(ids, r.commit(wt, v+"-"+string(rune('a'+i))+".txt", s, s))
	}
	r.git(r.dir, "worktree", "remove", wt)
	return ids
}

// hoist runs the git side of `bothways hoist` on a branch without a worktree.
func (r *repo) hoist(v string, commits []string) backend.Result {
	r.t.Helper()
	baseTip := r.tip("main")
	newBase, err := r.back.Replay(r.ctx, baseTip, commits)
	if err != nil {
		r.t.Fatal(err)
	}
	if err := r.back.MoveBranch(r.ctx, "main", baseTip, newBase, "test"); err != nil {
		r.t.Fatal(err)
	}
	res, err := r.back.Excise(r.ctx, backend.Excise{Branch: v, Tip: r.tip(v), Commits: commits, Base: newBase, Label: "bothways hoist " + v})
	if err != nil {
		r.t.Fatal(err)
	}
	return res
}

func TestExciseOnto(t *testing.T) {
	r := newRepo(t)
	own := r.variant("own-below", "own", "shared")
	first := r.variant("shared-first", "shared", "own")
	base := r.tip("main")

	// Nothing of the variant's own below the range: the base replaces it.
	if onto, err := ExciseOnto(r.ctx, r.dir, first[0], base); err != nil || onto != base {
		t.Errorf("shared first: onto = %s, %v; want base %s", onto, err, base)
	}
	// An own commit below: the parent replaces the range, so it survives.
	if onto, err := ExciseOnto(r.ctx, r.dir, own[1], base); err != nil || onto != own[0] {
		t.Errorf("own below: onto = %s, %v; want parent %s", onto, err, own[0])
	}
}

// The regression ExciseOnto was written for: hoisting a commit from the middle of
// a variant must not take the variant's commits below it along.
func TestExciseKeepsCommitsBelowAndAbove(t *testing.T) {
	r := newRepo(t)
	ids := r.variant("v", "circle: frame", "shared: typo", "circle: size")
	if res := r.hoist("v", ids[1:2]); res != (backend.Result{}) {
		t.Fatalf("result = %+v", res)
	}
	if got := r.subjects("main", "v"); !slices.Equal(got, []string{"circle: size", "circle: frame"}) {
		t.Errorf("main..v = %q", got)
	}
	if got := r.subjects("main~1", "main"); !slices.Equal(got, []string{"shared: typo"}) {
		t.Errorf("hoisted = %q", got)
	}
}

func TestExciseTip(t *testing.T) {
	r := newRepo(t)
	ids := r.variant("v", "circle: frame", "shared: typo")
	r.hoist("v", ids[1:])
	if got := r.subjects("main~1", "v"); !slices.Equal(got, []string{"circle: frame"}) {
		t.Errorf("v = %q, want only its own commit left", got)
	}
}

func TestExciseFirstLandsOnNewBase(t *testing.T) {
	r := newRepo(t)
	ids := r.variant("v", "shared: a", "shared: b", "square: corner")
	r.hoist("v", ids[:2])
	if ok, _ := gitx.IsAncestor(r.ctx, r.dir, r.tip("main"), r.tip("v")); !ok {
		t.Error("v is not stacked on the new base")
	}
	if got := r.subjects("main", "v"); !slices.Equal(got, []string{"square: corner"}) {
		t.Errorf("main..v = %q", got)
	}
}

func TestRestackConflictWithoutWorkspaceChangesNothing(t *testing.T) {
	r := newRepo(t)
	r.git(r.dir, "branch", "v", "main")
	vTip := r.commitOn("v", "page.txt", "circle", "circle: page")
	old := r.tip("main")
	onto := r.commit(r.dir, "page.txt", "tweak", "base: page")

	res, err := r.back.Restack(r.ctx, backend.Restack{Branch: "v", Tip: vTip, Upstream: old, Onto: onto, Label: "bothways sync v"})
	if err != nil || !res.Conflict {
		t.Fatalf("Restack = %+v, %v; want a conflict", res, err)
	}
	if r.tip("v") != vTip {
		t.Error("branch moved despite the conflict")
	}
	if wts, _ := r.back.Workspaces(r.ctx); len(wts) != 1 {
		t.Errorf("scratch worktree left behind: %+v", wts)
	}
}

func TestRestackKeepsUncommittedChanges(t *testing.T) {
	r := newRepo(t)
	r.variant("v", "circle: frame")
	wt := filepath.Join(r.tmp, "v")
	if err := r.back.AddWorkspace(r.ctx, wt, "v"); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(wt, "page.txt"), []byte("wip\n"), 0o644)
	old := r.tip("main")
	onto := r.commit(r.dir, "other.txt", "x", "base: other")

	res, err := r.back.Restack(r.ctx, backend.Restack{Branch: "v", Tip: r.tip("v"), Upstream: old, Onto: onto, Workspace: wt, Label: "bothways sync v"})
	if err != nil || res != (backend.Result{}) {
		t.Fatalf("Restack = %+v, %v", res, err)
	}
	if ok, _ := gitx.IsAncestor(r.ctx, r.dir, onto, r.tip("v")); !ok {
		t.Error("v was not restacked")
	}
	if b, _ := os.ReadFile(filepath.Join(wt, "page.txt")); string(b) != "wip\n" {
		t.Errorf("uncommitted change = %q", b)
	}
	if busy, _ := r.back.InConflict(r.ctx, wt); busy {
		t.Error("InConflict after a clean restack")
	}
}

func TestReplayConflict(t *testing.T) {
	r := newRepo(t)
	r.git(r.dir, "branch", "v", "main")
	c := r.commitOn("v", "page.txt", "circle", "circle: page")
	r.commit(r.dir, "page.txt", "tweak", "base: page")
	_, err := r.back.Replay(r.ctx, r.tip("main"), []string{c})
	var conflict *backend.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("Replay error = %v, want *ConflictError", err)
	}
	if wts, _ := r.back.Workspaces(r.ctx); len(wts) != 1 {
		t.Errorf("scratch worktree left behind: %+v", wts)
	}
}

func TestMergeConflictListsFiles(t *testing.T) {
	r := newRepo(t)
	r.git(r.dir, "branch", "a", "main")
	r.git(r.dir, "branch", "b", "main")
	ta := r.commitOn("a", "page.txt", "a", "a: page")
	tb := r.commitOn("b", "page.txt", "b", "b: page")
	combo := filepath.Join(r.tmp, "combo")
	if err := r.back.AddDetachedWorkspace(r.ctx, combo, r.tip("main")); err != nil {
		t.Fatal(err)
	}
	if err := r.back.Merge(r.ctx, combo, ta, "merge a"); err != nil {
		t.Fatal(err)
	}
	head, _ := r.back.Head(r.ctx, combo)
	err := r.back.Merge(r.ctx, combo, tb, "merge b")
	var conflict *backend.ConflictError
	if !errors.As(err, &conflict) || !slices.Equal(conflict.Files, []string{"page.txt"}) {
		t.Fatalf("Merge error = %v, want a conflict in page.txt", err)
	}
	if after, _ := r.back.Head(r.ctx, combo); after != head {
		t.Error("failed merge moved HEAD")
	}
	if dirty, _ := r.back.Dirty(r.ctx, combo, false); dirty {
		t.Error("failed merge left changes behind")
	}
}

// commitOn commits on branch in a throwaway worktree and returns the new tip.
func (r *repo) commitOn(branch, file, content, msg string) string {
	r.t.Helper()
	wt := filepath.Join(r.tmp, "commit-"+strings.ReplaceAll(branch, "/", "-"))
	r.git(r.dir, "worktree", "add", "-q", wt, branch)
	defer r.git(r.dir, "worktree", "remove", "--force", wt)
	return r.commit(wt, file, content, msg)
}
