package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/CoinkWang/bothways/internal/backend"
	"github.com/CoinkWang/bothways/internal/config"
	"github.com/CoinkWang/bothways/internal/gitx"
	"github.com/CoinkWang/bothways/internal/state"
	"github.com/CoinkWang/bothways/internal/worktree"
)

// reconcile makes the set of worktrees match the model: every variant of a
// materialized decision has one, deferred variants have none. beforeSetup,
// when set, runs after checkout and before the setup command.
func (a *app) reconcile(beforeSetup func(ref state.Ref, path string) error) ([]state.Ref, error) {
	if err := a.vcs.PruneWorkspaces(a.ctx); err != nil {
		return nil, err
	}
	st, err := a.load()
	if err != nil {
		return nil, err
	}
	wts, err := a.vcs.Workspaces(a.ctx)
	if err != nil {
		return nil, err
	}
	checkedOut := map[string]string{}
	registered := map[string]bool{}
	for _, wt := range wts {
		if wt.Branch != "" {
			checkedOut[wt.Branch] = wt.Path
		}
		registered[worktree.Canonical(wt.Path)] = true
	}
	var root string
	var created []state.Ref
	for _, d := range st.Ordered() {
		materialized := st.Materialized(d)
		for _, v := range d.Variants {
			ref := d.Ref(v)
			if path, ok := checkedOut[v.Branch]; ok {
				if !worktree.SamePath(path, v.Worktree) || v.State == state.Deferred {
					err := a.update(func(st *state.State) error {
						if v := st.Variant(ref); v != nil {
							v.Worktree = path
							if v.State == state.Deferred || v.State == "" {
								v.State = state.Idle
							}
						}
						return nil
					})
					if err != nil {
						return created, err
					}
				}
				continue
			}
			if v.Worktree != "" && registered[worktree.Canonical(v.Worktree)] && dirExists(v.Worktree) {
				continue // still there, just detached (e.g. stopped in a conflict)
			}
			if !materialized {
				if v.State != state.Deferred || v.Worktree != "" {
					err := a.update(func(st *state.State) error {
						if v := st.Variant(ref); v != nil {
							v.Worktree, v.State = "", state.Deferred
						}
						return nil
					})
					if err != nil {
						return created, err
					}
				}
				continue
			}
			if v.Worktree != "" {
				a.warnf("worktree for %s is gone (%s); recreating", ref, v.Worktree)
			}
			if root == "" {
				if root, err = a.worktreeRoot(st); err != nil {
					return created, err
				}
			}
			spec := worktree.Spec{
				RepoDir:  a.mainRoot,
				SeedDir:  a.seedDir(st),
				Path:     worktree.Path(root, d.ID, v.ID),
				Branch:   v.Branch,
				Decision: d.ID,
				Variant:  v.ID,
			}
			if err := a.materialize(ref, spec, beforeSetup); err != nil {
				return created, err
			}
			created = append(created, ref)
		}
	}
	return created, nil
}

func (a *app) materialize(ref state.Ref, spec worktree.Spec, beforeSetup func(state.Ref, string) error) error {
	if entries, err := os.ReadDir(spec.Path); err == nil && len(entries) > 0 {
		return userErrorf("cannot create worktree for %s: %s already exists and is not empty (remove it or run `bothways gc`)", ref, spec.Path)
	}
	if err := a.addWorktree(spec); err != nil {
		return userErrorf("creating worktree for %s: %v", ref, err)
	}
	worktree.ApplyPaths(a.ctx, spec, a.cfg.PathRules(), func(msg string) { a.warnf("%s: %s", ref, msg) })
	if beforeSetup != nil {
		if err := beforeSetup(ref, spec.Path); err != nil {
			return err
		}
	}
	fingerprint := ""
	if setup := a.cfg.Worktree.Setup; setup != "" && a.setupAllowed() {
		a.infof("%s: running setup: %s", ref, setup)
		if err := worktree.RunSetup(a.ctx, spec, setup, a.errw, a.errw); err != nil {
			a.warnf("%s: setup failed: %v (worktree is usable, dependencies may be missing)", ref, err)
		} else if fp, err := worktree.Fingerprint(spec.Path, a.cfg.Worktree.Fingerprint); err == nil {
			fingerprint = fp
		}
	}
	return a.update(func(st *state.State) error {
		if v := st.Variant(ref); v != nil {
			v.Worktree = spec.Path
			if v.State != state.Conflict {
				v.State = state.Idle
			}
			v.SetupFingerprint = fingerprint
		}
		return nil
	})
}

func (a *app) addWorktree(spec worktree.Spec) error {
	if err := os.MkdirAll(filepath.Dir(spec.Path), 0o755); err != nil {
		return err
	}
	return a.vcs.AddWorkspace(a.ctx, spec.Path, spec.Branch)
}

// ensureSetup runs the setup command in an existing worktree unless the
// fingerprint files still hash to what they did after the last successful
// setup there. A worktree is set up when it is created from its base,
// so as long as a variant leaves the fingerprint files as the base had them,
// up never reinstalls. With no fingerprint files configured setup always runs.
// Like materialize, a failing setup only degrades.
func (a *app) ensureSetup(ref state.Ref, dir string) error {
	setup := a.cfg.Worktree.Setup
	if setup == "" {
		return nil
	}
	fp, fpErr := worktree.Fingerprint(dir, a.cfg.Worktree.Fingerprint)
	st, err := a.load()
	if err != nil {
		return err
	}
	if v := st.Variant(ref); fpErr == nil && fp != "" && v != nil && v.SetupFingerprint == fp {
		a.infof("%s: setup skipped (fingerprint unchanged)", ref)
		return nil
	}
	if !a.setupAllowed() {
		return nil
	}
	spec := worktree.Spec{RepoDir: a.mainRoot, Path: dir, Decision: ref.Decision, Variant: ref.Variant}
	a.infof("%s: running setup: %s", ref, setup)
	if err := worktree.RunSetup(a.ctx, spec, setup, a.errw, a.errw); err != nil {
		a.warnf("%s: setup failed: %v (dependencies may be missing)", ref, err)
		return nil
	}
	// Setup may rewrite a lockfile; record what it left behind.
	if fp, fpErr = worktree.Fingerprint(dir, a.cfg.Worktree.Fingerprint); fpErr != nil {
		return nil
	}
	return a.update(func(st *state.State) error {
		if v := st.Variant(ref); v != nil {
			v.SetupFingerprint = fp
		}
		return nil
	})
}

// Sync outcomes.
const (
	outUpToDate = "up-to-date"
	outSynced   = "synced"
	outConflict = "conflict"
	outSkipped  = "skipped"
	outFailed   = "failed"
)

type syncResult struct {
	Ref     state.Ref `json:"-"`
	Name    string    `json:"name"`
	Outcome string    `json:"outcome"`
	Detail  string    `json:"detail,omitempty"`
}

func (r syncResult) ok() bool { return r.Outcome == outUpToDate || r.Outcome == outSynced }

// sync restacks variants on their parent's current tip. targets nil
// means every open variant. Parents are processed before children; a child
// whose parent did not sync cleanly is skipped. Per-variant trouble is
// reported in the results, not as an error.
func (a *app) sync(targets []state.Ref) ([]syncResult, error) {
	st, err := a.prepare()
	if err != nil {
		return nil, err
	}
	var wanted map[state.Ref]bool
	if targets != nil {
		wanted = map[state.Ref]bool{}
		for _, t := range targets {
			wanted[t] = true
		}
	}
	notClean := map[state.Ref]bool{}
	var results []syncResult
	for _, d := range st.Ordered() {
		for _, v := range d.Variants {
			ref := d.Ref(v)
			if d.Under != nil {
				if notClean[*d.Under] {
					notClean[ref] = true
					if wanted == nil || wanted[ref] {
						results = append(results, syncResult{ref, ref.String(), outSkipped, "parent " + d.Under.String() + " did not sync"})
					}
					continue
				}
				if pv := st.Variant(*d.Under); pv != nil && pv.State == state.Conflict {
					notClean[ref] = true
					if wanted == nil || wanted[ref] {
						results = append(results, syncResult{ref, ref.String(), outSkipped, "parent " + d.Under.String() + " is in conflict"})
					}
					continue
				}
			}
			if wanted != nil && !wanted[ref] {
				continue
			}
			r := a.syncOne(st, d, v)
			r.Ref, r.Name = ref, ref.String()
			if !r.ok() {
				notClean[ref] = true
			}
			results = append(results, r)
		}
	}
	return results, nil
}

func (a *app) syncOne(st *state.State, d *state.Decision, v *state.Variant) syncResult {
	ref := d.Ref(v)
	fail := func(format string, args ...any) syncResult {
		return syncResult{Outcome: outFailed, Detail: fmt.Sprintf(format, args...)}
	}
	parent, err := a.parentBranch(st, d)
	if err != nil {
		return fail("%v", err)
	}
	onto, err := a.tipOf(parent)
	if err != nil {
		return fail("%v", err)
	}
	tip, ok, err := gitx.BranchTip(a.ctx, a.mainRoot, v.Branch)
	if err != nil {
		return fail("%v", err)
	}
	if !ok {
		return fail("branch %s is missing; run `bothways doctor`", v.Branch)
	}
	hasWorktree := v.Worktree != "" && dirExists(v.Worktree)
	words := a.vcs.Vocab()
	if v.State == state.Conflict && hasWorktree {
		return syncResult{Outcome: outConflict, Detail: words.Stopped + " in progress in " + v.Worktree + "; resolve, then " + words.Continue}
	}
	setSynced := func(baseTip string) error {
		return a.update(func(st *state.State) error {
			if v := st.Variant(ref); v != nil {
				v.BaseTip, v.SyncOnto = baseTip, ""
				if v.State == state.Conflict {
					v.State = state.Idle
					if v.Worktree == "" {
						v.State = state.Deferred
					}
				}
			}
			return nil
		})
	}

	// Already stacked on the parent tip (possibly moved there along with
	// another variant it is nested in).
	if contains, err := gitx.IsAncestor(a.ctx, a.mainRoot, onto, tip); err != nil {
		return fail("%v", err)
	} else if contains {
		if v.BaseTip != onto || v.State == state.Conflict {
			if err := setSynced(onto); err != nil {
				return fail("%v", err)
			}
		}
		return syncResult{Outcome: outUpToDate}
	}

	from := v.BaseTip
	stacked := false
	if from != "" {
		stacked, _ = gitx.IsAncestor(a.ctx, a.mainRoot, from, tip)
	}
	if !stacked {
		mb, err := gitx.MergeBase(a.ctx, a.mainRoot, onto, tip)
		if err != nil {
			return fail("cannot find where %s stacks on %s: %v", v.Branch, parent, err)
		}
		if v.BaseTip != "" {
			a.warnf("%s: recorded base %s is not in its history; using merge-base %s", ref, short(v.BaseTip), short(mb))
		}
		from = mb
	}

	// A deferred variant with no commits of its own has nothing to rewrite.
	if !hasWorktree && tip == from {
		if err := a.vcs.MoveBranch(a.ctx, v.Branch, tip, onto, "bothways: sync "+ref.String()); err != nil {
			return fail("%v", err)
		}
		if err := setSynced(onto); err != nil {
			return fail("%v", err)
		}
		return syncResult{Outcome: outSynced}
	}

	if a.skipDirty && hasWorktree {
		// Someone may be working in there right now: a background sync must not
		// rewrite files under an editor or race their next commit for index.lock.
		dirty, err := a.vcs.Dirty(a.ctx, v.Worktree, true)
		if err != nil {
			return fail("%v", err)
		}
		if dirty {
			return syncResult{Outcome: outSkipped, Detail: "uncommitted changes in " + v.Worktree + "; left alone, run `bothways sync` when done"}
		}
	}

	req := backend.Restack{Branch: v.Branch, Tip: tip, Upstream: from, Onto: onto, Label: "bothways sync " + ref.String()}
	if hasWorktree {
		req.Workspace = v.Worktree
	}
	res, err := a.vcs.Restack(a.ctx, req)
	var scratchErr *backend.ScratchError
	var saveErr *backend.SaveError
	switch {
	case errors.As(err, &scratchErr):
		return fail("%v", scratchErr.Err)
	case errors.As(err, &saveErr):
		return fail("%s uncommitted changes: %v", words.Saving, saveErr.Err)
	case err != nil:
		return fail("%v", err)
	}
	if res.Conflict {
		err := a.update(func(st *state.State) error {
			if v := st.Variant(ref); v != nil {
				v.State = state.Conflict
				v.SyncOnto = ""
				if hasWorktree {
					v.SyncOnto = onto
				}
			}
			return nil
		})
		if err != nil {
			return fail("%v", err)
		}
		detail := "resolve in " + v.Worktree + ", then " + words.Continue
		if !hasWorktree {
			detail = "deferred variant does not rebase cleanly onto " + parent + "; it will be retried on the next sync"
		}
		if res.Saved {
			detail += " (" + words.Saved + ")"
		}
		return syncResult{Outcome: outConflict, Detail: detail}
	}
	if err := setSynced(onto); err != nil {
		return fail("%v", err)
	}
	if res.NotRestored {
		return fail("synced, but re-applying uncommitted changes in %s conflicted; resolve and %s", v.Worktree, words.DropSaved)
	}
	return syncResult{Outcome: outSynced}
}

// reportSync prints results and returns how many need attention. With
// problemsOnly, clean outcomes are not printed.
func reportSync(w io.Writer, results []syncResult, problemsOnly bool) int {
	problems := 0
	for _, r := range results {
		if !r.ok() {
			problems++
		} else if problemsOnly {
			continue
		}
		if r.Detail != "" {
			fmt.Fprintf(w, "%s\t%s\t%s\n", r.Name, r.Outcome, r.Detail)
		} else {
			fmt.Fprintf(w, "%s\t%s\n", r.Name, r.Outcome)
		}
	}
	return problems
}

// moveBranch fast-forwards (or rewinds) branch from oldTip to newTip with a
// compare-and-swap, then brings a clean checkout of it along.
func (a *app) moveBranch(branch, oldTip, newTip, msg string) error {
	wt, hasWT, err := a.worktreeOf(branch)
	if err != nil {
		return err
	}
	dirty := false
	if hasWT {
		// Must be checked before the ref moves: afterwards the index differs from HEAD.
		if dirty, err = a.vcs.Dirty(a.ctx, wt, false); err != nil {
			return err
		}
	}
	if err := a.vcs.MoveBranch(a.ctx, branch, oldTip, newTip, msg); err != nil {
		return partialErrorf("%s moved concurrently: %v", branch, err)
	}
	if !hasWT {
		return nil
	}
	hint := a.vcs.FollowCommand(wt, short(oldTip), short(newTip))
	if dirty {
		a.warnf("%s has uncommitted changes; moved %s only. To update its files run:\n  %s", wt, branch, hint)
		return nil
	}
	if err := a.vcs.FollowBranch(a.ctx, wt, oldTip, newTip); err != nil {
		a.warnf("moved %s but could not update files in %s: %v\n  %s", branch, wt, err, hint)
	}
	return nil
}

// subtreeRefs lists ref and every variant nested under it.
func subtreeRefs(st *state.State, ref state.Ref) []state.Ref {
	refs := []state.Ref{ref}
	for _, d := range st.Subtree(ref) {
		for _, v := range d.Variants {
			refs = append(refs, d.Ref(v))
		}
	}
	return refs
}

// checkRemovable refuses to discard worktrees with uncommitted or untracked
// files unless force is set.
func (a *app) checkRemovable(st *state.State, refs []state.Ref, force bool) error {
	if force {
		return nil
	}
	var dirty []string
	for _, ref := range refs {
		v := st.Variant(ref)
		if v == nil || v.Worktree == "" || !dirExists(v.Worktree) {
			continue
		}
		isDirty, err := a.vcs.Dirty(a.ctx, v.Worktree, true)
		if err != nil {
			return err
		}
		if isDirty {
			dirty = append(dirty, fmt.Sprintf("%s (%s)", ref, v.Worktree))
		}
	}
	if len(dirty) > 0 {
		return userErrorf("uncommitted or untracked files would be lost in: %s; commit or stash them, or pass --force", strings.Join(dirty, ", "))
	}
	return nil
}

// checkDownScripts refuses up front to remove variants whose deployment a down
// script has to release when the config is not trusted: stopping halfway
// through pick or drop would leave the environment behind.
func (a *app) checkDownScripts(st *state.State, refs []state.Ref) error {
	if a.cfg.Preview.Down == "" {
		return nil
	}
	for _, ref := range refs {
		v := st.Variant(ref)
		if v != nil && v.Preview != nil && v.Preview.Mode == config.ModeDeploy && v.Preview.State != state.PreviewDown {
			return a.requireTrust("run the down script for " + ref.String())
		}
	}
	return nil
}

// removeWorktree deletes a variant worktree and its empty decision directory.
// Whatever preview runs out of it is stopped first, so pick, drop and gc never
// leave an orphan process or deployment behind.
func (a *app) removeWorktree(path string) error {
	if path == "" {
		return nil
	}
	if err := a.stopPreviewAt(path); err != nil {
		return err
	}
	if !dirExists(path) {
		return nil
	}
	if err := a.vcs.RemoveWorkspace(a.ctx, path); err != nil {
		return err
	}
	os.Remove(filepath.Dir(path)) // only succeeds when empty
	return nil
}

func (a *app) stopPreviewAt(path string) error {
	st, err := a.load()
	if err != nil {
		return err
	}
	for _, d := range st.Decisions {
		for _, v := range d.Variants {
			if v.Preview != nil && v.Preview.State != state.PreviewDown && worktree.SamePath(v.Worktree, path) {
				_, err := a.stopPreview(d.Ref(v))
				return err
			}
		}
	}
	return nil
}

// reject removes ref and everything nested under it: worktrees go away,
// branches move to bothways/rejected/ (or are deleted with purge).
func (a *app) reject(st *state.State, ref state.Ref, purge bool) ([]state.Rejected, error) {
	var out []state.Rejected
	for _, r := range subtreeRefs(st, ref) {
		v := st.Variant(r)
		if v == nil {
			continue
		}
		if err := a.removeWorktree(v.Worktree); err != nil {
			return out, fmt.Errorf("removing worktree of %s: %w", r, err)
		}
		if _, ok, _ := gitx.BranchTip(a.ctx, a.mainRoot, v.Branch); !ok {
			continue
		}
		if purge {
			if err := a.vcs.DeleteBranch(a.ctx, v.Branch); err != nil {
				return out, err
			}
			continue
		}
		name, err := a.rejectedName(r)
		if err != nil {
			return out, err
		}
		if err := a.vcs.RenameBranch(a.ctx, v.Branch, name); err != nil {
			return out, err
		}
		out = append(out, state.Rejected{Decision: r.Decision, Variant: r.Variant, Branch: name, At: now()})
	}
	return out, nil
}

func (a *app) rejectedName(ref state.Ref) (string, error) {
	base := state.RejectedPrefix + ref.Decision + "/" + ref.Variant
	name := base
	for i := 2; ; i++ {
		_, exists, err := gitx.BranchTip(a.ctx, a.mainRoot, name)
		if err != nil {
			return "", err
		}
		if !exists {
			return name, nil
		}
		name = fmt.Sprintf("%s-%d", base, i)
	}
}
