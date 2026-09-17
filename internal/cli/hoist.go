package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/CoinkWang/bothways/internal/backend"
	"github.com/CoinkWang/bothways/internal/gitx"
	"github.com/CoinkWang/bothways/internal/state"
)

func newHoistCmd() *cobra.Command {
	var from string
	cmd := &cobra.Command{
		Use:   "hoist <commit> [--from <decision>=<variant>]",
		Short: "Move a commit from a variant down to the base",
		Long: `Move work that was committed in a variant worktree but belongs to everyone
onto the base branch: cherry-pick it there, drop it from the variant, then
restack the other variants.

<commit> is a single revision or an <a>..<b> range. It must be a commit of the
variant and must not already be on the base.`,
		Args: wrapArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHoist(cmd, args[0], from)
		},
	}
	cmd.Flags().StringVar(&from, "from", "", "variant the commit belongs to; inferred when exactly one has it")
	return cmd
}

// runHoist does, in order: replay the commits onto the base tip out of sight,
// CAS-move the base, excise the commits from the source variant, and let sync
// restack everything onto the new base.
func runHoist(cmd *cobra.Command, arg, from string) error {
	a, err := openApp(cmd)
	if err != nil {
		return err
	}
	st, err := a.prepare()
	if err != nil {
		return err
	}
	base := st.BaseBranch
	if base == "" {
		return userErrorf("no base branch recorded; run `bothways doctor`")
	}
	commits, err := a.hoistCommits(arg)
	if err != nil {
		return err
	}
	baseTip, err := a.tipOf(base)
	if err != nil {
		return err
	}
	for _, c := range commits {
		on, err := gitx.IsAncestor(a.ctx, a.mainRoot, c, baseTip)
		if err != nil {
			return err
		}
		if on {
			return userErrorf("%s is already on %s", short(c), base)
		}
	}
	// The parent of the oldest commit is where the variant is cut back to
	// when the hoisted range does not start at the base; resolve it early so
	// an unusable range fails before anything moves.
	if _, err := gitx.RevParse(a.ctx, a.mainRoot, commits[0]+"^"); err != nil {
		return userErrorf("%s has no single parent; hoist a plain range of commits", short(commits[0]))
	}
	ref, err := a.hoistSource(st, from, commits)
	if err != nil {
		return err
	}
	d, v := st.Decision(ref.Decision), st.Variant(ref)
	if v.State == state.Conflict {
		return partialErrorf("cannot hoist from %s: it is in conflict; resolve the %s in %s first", ref, a.vcs.Vocab().Stopped, orDash(v.Worktree))
	}
	if err := a.checkHoistable(st, d, v, ref, commits); err != nil {
		return err
	}

	// 1. Replay the commits on top of the base without touching the base
	// worktree or the variant worktree.
	newBase, err := a.vcs.Replay(a.ctx, baseTip, commits)
	var scratchErr *backend.ScratchError
	var conflictErr *backend.ConflictError
	switch {
	case errors.As(err, &scratchErr):
		return envErrorf("%v", scratchErr)
	case errors.As(err, &conflictErr):
		return partialErrorf("%s does not apply cleanly onto %s: %v", nCommits(len(commits)), base, conflictErr)
	case err != nil:
		return err
	}

	// 2. Publish them (compare-and-swap on the old tip).
	if err := a.moveBranch(base, baseTip, newBase, "bothways: hoist "+ref.String()); err != nil {
		return err
	}
	fmt.Fprintf(a.out, "hoisted %s from %s: %s %s -> %s\n", nCommits(len(commits)), ref, base, short(baseTip), short(newBase))

	// 3. Take them out of the variant. This may already land the variant on
	// the new base, or only remove the range and leave the restack to sync.
	dropErr := a.dropCommits(ref, v, newBase, commits)

	// 4. Everything else (and the source variant, when step 3 left it behind).
	results, err := a.sync(nil)
	if err != nil {
		return err
	}
	problems := reportSync(a.errw, results, true)
	if dropErr != nil {
		return dropErr
	}
	if problems > 0 {
		return partialErrorf("hoisted %s, but %d variant(s) did not sync cleanly", nCommits(len(commits)), problems)
	}
	return nil
}

// hoistCommits resolves arg to the commits to move, oldest first. Revisions
// are resolved from the working directory so that `HEAD~1` means what it does
// in the variant worktree the command is run from.
func (a *app) hoistCommits(arg string) ([]string, error) {
	if strings.Contains(arg, "...") {
		return nil, userErrorf("hoist takes one commit or an <a>..<b> range, not a symmetric difference")
	}
	if !strings.Contains(arg, "..") {
		oid, err := a.vcs.ResolveCommit(a.ctx, a.cwd, arg)
		if err != nil {
			return nil, userErrorf("cannot resolve %q to a commit", arg)
		}
		return []string{oid}, nil
	}
	commits, err := a.vcs.ResolveRange(a.ctx, a.cwd, arg)
	if err != nil {
		return nil, userErrorf("cannot resolve %q to a range of commits", arg)
	}
	if len(commits) == 0 {
		return nil, userErrorf("%s is empty: no commits to hoist", arg)
	}
	// A range with a fork in it cannot be cut out of the variant with one
	// rewrite, so refuse it rather than rewriting more than was asked for.
	n, err := gitx.Count(a.ctx, a.mainRoot, commits[0]+"^", commits[len(commits)-1])
	if err != nil || n != len(commits) {
		return nil, userErrorf("%s is not a contiguous range of commits", arg)
	}
	return commits, nil
}

// hoistSource picks the variant the commits are taken out of: the one whose
// branch has them while its parent's branch does not. Without that second
// half every variant nested under the real owner would match too.
func (a *app) hoistSource(st *state.State, from string, commits []string) (state.Ref, error) {
	if from != "" {
		ref, err := st.Resolve(from)
		if err != nil {
			return state.Ref{}, userErrorf("--from: %v", err)
		}
		return ref, nil
	}
	newest := commits[len(commits)-1]
	var found []state.Ref
	for _, d := range st.Ordered() {
		for _, v := range d.Variants {
			owns, err := a.ownsCommit(st, d, v, newest)
			if err != nil {
				return state.Ref{}, err
			}
			if owns {
				found = append(found, d.Ref(v))
			}
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return state.Ref{}, userErrorf("no open variant has %s; pass --from <decision>=<variant>", short(newest))
	}
	names := make([]string, len(found))
	for i, r := range found {
		names[i] = r.String()
	}
	return state.Ref{}, userErrorf("%s is on several variants (%s); pass --from <decision>=<variant>", short(newest), strings.Join(names, ", "))
}

func (a *app) ownsCommit(st *state.State, d *state.Decision, v *state.Variant, commit string) (bool, error) {
	tip, ok, err := gitx.BranchTip(a.ctx, a.mainRoot, v.Branch)
	if err != nil || !ok {
		return false, err
	}
	on, err := gitx.IsAncestor(a.ctx, a.mainRoot, commit, tip)
	if err != nil || !on {
		return false, err
	}
	parent, err := a.parentBranch(st, d)
	if err != nil {
		return false, err
	}
	parentTip, ok, err := gitx.BranchTip(a.ctx, a.mainRoot, parent)
	if err != nil {
		return false, err
	}
	if !ok {
		return true, nil // nothing to have inherited it from
	}
	inherited, err := gitx.IsAncestor(a.ctx, a.mainRoot, commit, parentTip)
	return !inherited, err
}

// checkHoistable rejects commits that are not the variant's own work. Being
// on the base is checked earlier, so the parent case only fires for nested
// decisions.
func (a *app) checkHoistable(st *state.State, d *state.Decision, v *state.Variant, ref state.Ref, commits []string) error {
	tip, err := a.tipOf(v.Branch)
	if err != nil {
		return err
	}
	parent, err := a.parentBranch(st, d)
	if err != nil {
		return err
	}
	parentTip, err := a.tipOf(parent)
	if err != nil {
		return err
	}
	for _, c := range commits {
		on, err := gitx.IsAncestor(a.ctx, a.mainRoot, c, tip)
		if err != nil {
			return err
		}
		if !on {
			return userErrorf("%s is not on %s (%s)", short(c), ref, v.Branch)
		}
		inherited, err := gitx.IsAncestor(a.ctx, a.mainRoot, c, parentTip)
		if err != nil {
			return err
		}
		if inherited {
			return userErrorf("%s is not a commit of %s: it comes from %s; hoist it from there", short(c), ref, parent)
		}
	}
	return nil
}

// dropCommits excises commits from the variant branch now that newBase has
// them. With a worktree the rewrite happens there, because that is where a
// conflict has to be resolved.
func (a *app) dropCommits(ref state.Ref, v *state.Variant, newBase string, commits []string) error {
	req := backend.Excise{Branch: v.Branch, Commits: commits, Base: newBase, Label: "bothways hoist " + ref.String()}
	if v.Worktree != "" && dirExists(v.Worktree) {
		req.Workspace = v.Worktree
	} else {
		tip, err := a.tipOf(v.Branch)
		if err != nil {
			return err
		}
		req.Tip = tip
	}
	words := a.vcs.Vocab()
	res, err := a.vcs.Excise(a.ctx, req)
	var scratchErr *backend.ScratchError
	var saveErr *backend.SaveError
	switch {
	case errors.As(err, &scratchErr):
		return envErrorf("%v", scratchErr)
	case errors.As(err, &saveErr):
		return partialErrorf("%s uncommitted changes in %s: %v", words.Saving, req.Workspace, saveErr.Err)
	case err != nil:
		return partialErrorf("removing the hoisted work from %s: %v", ref, err)
	case res.NotRestored:
		return partialErrorf("%s no longer has the hoisted work, but re-applying its uncommitted changes conflicted; resolve in %s and %s", ref, req.Workspace, words.DropSaved)
	case !res.Conflict:
		return nil
	case req.Workspace == "":
		return partialErrorf("%s does not rebase cleanly without the hoisted work; it keeps its own copy until the conflict is resolved", ref)
	}
	// SyncOnto stays empty: the rewrite lands the variant on its old base, not
	// on the new one, so the next sync is what moves it forward.
	err = a.update(func(st *state.State) error {
		if v := st.Variant(ref); v != nil {
			v.State, v.SyncOnto = state.Conflict, ""
		}
		return nil
	})
	if err != nil {
		return err
	}
	detail := "resolve in " + req.Workspace + ", then " + words.Continue + " and `bothways sync`"
	if res.Saved {
		detail += " (" + words.Saved + ")"
	}
	return partialErrorf("the base has the hoisted work, but removing it from %s conflicted; %s", ref, detail)
}

func nCommits(n int) string {
	if n == 1 {
		return "1 commit"
	}
	return fmt.Sprintf("%d commits", n)
}
