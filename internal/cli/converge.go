package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/CoinkWang/bothways/internal/gitx"
	"github.com/CoinkWang/bothways/internal/state"
)

func newSyncCmd() *cobra.Command {
	var asJSON, skipDirty bool
	cmd := &cobra.Command{
		Use:   "sync [<decision>=<variant> ...]",
		Short: "Restack variants onto the latest tip of their base (rebase --onto)",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openApp(cmd)
			if err != nil {
				return err
			}
			a.skipDirty = skipDirty
			st, err := a.prepare()
			if err != nil {
				return err
			}
			var targets []state.Ref
			for _, arg := range args {
				ref, err := st.Resolve(arg)
				if err != nil {
					return userErrorf("%v", err)
				}
				targets = append(targets, ref)
			}
			results, err := a.sync(targets)
			if err != nil {
				return err
			}
			if _, err := a.reconcile(nil); err != nil {
				return err
			}
			problems := 0
			if asJSON {
				for _, r := range results {
					if !r.ok() {
						problems++
					}
				}
				if results == nil {
					results = []syncResult{}
				}
				if err := writeJSON(a.out, map[string]any{"schema_version": state.SchemaVersion, "results": results}); err != nil {
					return err
				}
			} else {
				problems = reportSync(a.out, results, false)
			}
			if problems > 0 {
				return partialErrorf("%d variant(s) did not sync cleanly", problems)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	cmd.Flags().BoolVar(&skipDirty, "skip-dirty", false, "leave variants with uncommitted or untracked changes alone instead of stashing around the rebase")
	return cmd
}

func newPickCmd() *cobra.Command {
	var purge, force bool
	cmd := &cobra.Command{
		Use:   "pick <decision>=<variant>",
		Short: "Fast-forward the base to a variant and reclaim its siblings",
		Args:  wrapArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPick(cmd, args[0], purge, force)
		},
	}
	cmd.Flags().BoolVar(&purge, "purge", false, "delete rejected branches instead of keeping them under bothways/rejected/")
	cmd.Flags().BoolVar(&force, "force", false, "discard uncommitted files in worktrees that are removed")
	return cmd
}

// runPick does, in order: sync the winner, assert fast-forward, CAS-move the
// base, restack everything else, reclaim the winner's worktree and branch,
// and reject siblings together with everything nested under them.
func runPick(cmd *cobra.Command, arg string, purge, force bool) error {
	a, err := openApp(cmd)
	if err != nil {
		return err
	}
	st, err := a.prepare()
	if err != nil {
		return err
	}
	ref, err := st.Resolve(arg)
	if err != nil {
		return userErrorf("%v", err)
	}
	d := st.Decision(ref.Decision)
	winner := d.Variant(ref.Variant)
	if d.Under != nil {
		return userErrorf("%s is nested under %s; pick %s first", d.ID, d.Under, d.Under)
	}
	if winner.State == state.Conflict {
		return partialErrorf("cannot pick %s: it is in conflict; resolve the %s in %s first", ref, a.vcs.Vocab().Stopped, orDash(winner.Worktree))
	}
	var losers []state.Ref
	for _, v := range d.Variants {
		if v.ID != winner.ID {
			losers = append(losers, subtreeRefs(st, d.Ref(v))...)
		}
	}
	if err := a.checkRemovable(st, append([]state.Ref{ref}, losers...), force); err != nil {
		return err
	}
	if err := a.checkDownScripts(st, append([]state.Ref{ref}, losers...)); err != nil {
		return err
	}

	// 1. Make sure the winner sits on the current base tip.
	results, err := a.sync([]state.Ref{ref})
	if err != nil {
		return err
	}
	if reportSync(a.errw, results, true) > 0 {
		return partialErrorf("cannot pick %s: it does not sync cleanly onto %s", ref, st.BaseBranch)
	}
	base := st.BaseBranch
	baseTip, err := a.tipOf(base)
	if err != nil {
		return err
	}
	winnerTip, err := a.tipOf(winner.Branch)
	if err != nil {
		return err
	}
	mb, err := gitx.MergeBase(a.ctx, a.mainRoot, baseTip, winnerTip)
	if err != nil {
		return err
	}
	if mb != baseTip {
		return partialErrorf("cannot pick %s: %s is not a fast-forward of %s", ref, winner.Branch, base)
	}

	// 2. Fast-forward the base (compare-and-swap on the old tip).
	if err := a.moveBranch(base, baseTip, winnerTip, "bothways: pick "+ref.String()); err != nil {
		return err
	}

	// 3. The winner's content now lives in the base.
	if st, err = a.load(); err != nil {
		return err
	}
	if err := a.removeWorktree(winner.Worktree); err != nil {
		return err
	}
	if err := a.vcs.DeleteBranch(a.ctx, winner.Branch); err != nil {
		return err
	}

	// 4. Siblings and everything nested under them.
	var rejected []state.Rejected
	for _, v := range d.Variants {
		if v.ID == winner.ID {
			continue
		}
		entries, err := a.reject(st, d.Ref(v), purge)
		rejected = append(rejected, entries...)
		if err != nil {
			return err
		}
	}

	promoted := st.Children(ref)
	err = a.update(func(st *state.State) error {
		d := st.Decision(ref.Decision)
		if d != nil {
			for _, v := range d.Variants {
				if v.ID == ref.Variant {
					continue
				}
				for _, sub := range st.Subtree(d.Ref(v)) {
					st.RemoveDecision(sub.ID)
				}
			}
		}
		for _, c := range st.Children(ref) {
			c.Under = nil
		}
		st.RemoveDecision(ref.Decision)
		st.Picked = append(st.Picked, state.Picked{Decision: ref.Decision, Variant: ref.Variant, Tip: winnerTip, At: now()})
		st.Rejected = append(st.Rejected, rejected...)
		return nil
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(a.out, "picked %s: %s %s -> %s\n", ref, base, short(baseTip), short(winnerTip))
	for _, r := range rejected {
		fmt.Fprintf(a.out, "rejected %s=%s -> %s\n", r.Decision, r.Variant, r.Branch)
	}

	if st, err = a.load(); err != nil {
		return err
	}
	for _, c := range promoted {
		if d := st.Decision(c.ID); d != nil {
			if err := a.writeDescriptions(st, d); err != nil {
				return err
			}
		}
	}
	// Decisions nested under the winner become top-level and get worktrees.
	if _, err := a.reconcile(nil); err != nil {
		return err
	}

	// 5. Restack every remaining open variant onto the new base.
	results, err = a.sync(nil)
	if err != nil {
		return err
	}
	if n := reportSync(a.errw, results, true); n > 0 {
		return partialErrorf("picked %s, but %d remaining variant(s) did not sync cleanly", ref, n)
	}
	return nil
}

func newDropCmd() *cobra.Command {
	var purge, force bool
	cmd := &cobra.Command{
		Use:   "drop <decision>=<variant>",
		Short: "Remove one variant (and anything nested under it) without deciding",
		Args:  wrapArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openApp(cmd)
			if err != nil {
				return err
			}
			st, err := a.prepare()
			if err != nil {
				return err
			}
			ref, err := st.Resolve(args[0])
			if err != nil {
				return userErrorf("%v", err)
			}
			if err := a.checkRemovable(st, subtreeRefs(st, ref), force); err != nil {
				return err
			}
			if err := a.checkDownScripts(st, subtreeRefs(st, ref)); err != nil {
				return err
			}
			rejected, err := a.reject(st, ref, purge)
			if err != nil {
				return err
			}
			var changed *state.Decision
			err = a.update(func(st *state.State) error {
				for _, sub := range st.Subtree(ref) {
					st.RemoveDecision(sub.ID)
				}
				if d := st.Decision(ref.Decision); d != nil {
					d.RemoveVariant(ref.Variant)
					switch {
					case len(d.Variants) == 0:
						st.RemoveDecision(d.ID)
					case d.Default == ref.Variant:
						d.Default = d.Variants[0].ID
						changed = d
					}
				}
				st.Rejected = append(st.Rejected, rejected...)
				return nil
			})
			if err != nil {
				return err
			}
			if changed != nil {
				a.infof("%s: default is now %s", changed.ID, changed.Default)
				st, err := a.load()
				if err != nil {
					return err
				}
				if d := st.Decision(changed.ID); d != nil {
					if err := a.writeDescriptions(st, d); err != nil {
						return err
					}
				}
			}
			fmt.Fprintf(a.out, "dropped %s\n", ref)
			for _, r := range rejected {
				fmt.Fprintf(a.out, "rejected %s=%s -> %s\n", r.Decision, r.Variant, r.Branch)
			}
			_, err = a.reconcile(nil)
			return err
		},
	}
	cmd.Flags().BoolVar(&purge, "purge", false, "delete the branch instead of keeping it under bothways/rejected/")
	cmd.Flags().BoolVar(&force, "force", false, "discard uncommitted files in removed worktrees")
	return cmd
}
