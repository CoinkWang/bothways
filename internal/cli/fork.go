package cli

import (
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/CoinkWang/bothways/internal/backend"
	"github.com/CoinkWang/bothways/internal/gitx"
	"github.com/CoinkWang/bothways/internal/state"
)

type forkOptions struct {
	alts         []string
	question     string
	defaultAlt   string
	under        string
	carry        string
	split        string
	force        bool
	decisionID   string
	parentBranch string
	underRef     *state.Ref
}

func newForkCmd() *cobra.Command {
	var o forkOptions
	cmd := &cobra.Command{
		Use:   "fork <decision> --alt <a> --alt <b> [--q ...] [--default a] [--under d0=x] [--carry a | --split <rev>]",
		Short: "Open a decision with one variant branch and worktree per alternative",
		Args:  wrapArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			o.decisionID = args[0]
			return runFork(cmd, &o)
		},
	}
	f := cmd.Flags()
	f.StringArrayVar(&o.alts, "alt", nil, "variant id (repeatable; the first is the default)")
	f.StringVar(&o.question, "q", "", "the question this decision answers")
	f.StringVar(&o.defaultAlt, "default", "", "default variant (defaults to the first --alt)")
	f.StringVar(&o.under, "under", "", "nest under an open variant <decision>=<variant>")
	f.StringVar(&o.carry, "carry", "", "move uncommitted changes of the parent worktree into this variant")
	f.StringVar(&o.split, "split", "", "turn <rev>..HEAD of the parent branch into the first variant")
	f.BoolVar(&o.force, "force", false, "skip the similar-question check")
	return cmd
}

func runFork(cmd *cobra.Command, o *forkOptions) error {
	if err := state.ValidateID("decision", o.decisionID); err != nil {
		return userErrorf("%v", err)
	}
	if err := validateAlts(o.alts); err != nil {
		return err
	}
	if o.defaultAlt == "" {
		o.defaultAlt = o.alts[0]
	} else if !slices.Contains(o.alts, o.defaultAlt) {
		return userErrorf("--default %q is not one of the --alt values", o.defaultAlt)
	}
	if o.carry != "" && o.split != "" {
		return userErrorf("--carry and --split are mutually exclusive")
	}
	if o.carry != "" && !slices.Contains(o.alts, o.carry) {
		return userErrorf("--carry %q is not one of the --alt values", o.carry)
	}

	a, err := openApp(cmd)
	if err != nil {
		return err
	}
	if err := a.ensureInit(); err != nil {
		return err
	}
	st, err := a.prepare()
	if err != nil {
		return err
	}
	if st.Decision(o.decisionID) != nil {
		return userErrorf("decision %q is already open; add variants with `bothways add %s --alt <id>`", o.decisionID, o.decisionID)
	}
	if existing, err := gitx.ListRefs(a.ctx, a.mainRoot, "refs/heads/"+state.BranchPrefix+o.decisionID+"/"); err != nil {
		return err
	} else if len(existing) > 0 {
		return userErrorf("branches under %s%s/ already exist; run `bothways doctor` to adopt them", state.BranchPrefix, o.decisionID)
	}
	if err := checkSimilar(st, o.question, a.cfg.Fork.SimilarityThreshold, o.force); err != nil {
		return err
	}
	base, err := a.forkParent(st, o)
	if err != nil {
		return err
	}

	parentTip, err := a.tipOf(o.parentBranch)
	if err != nil {
		return err
	}
	decision := &state.Decision{
		ID:        o.decisionID,
		Question:  strings.Join(strings.Fields(o.question), " "),
		Default:   o.defaultAlt,
		Under:     o.underRef,
		CreatedAt: now(),
	}
	materialized := st.Materialized(decision)

	parentWT, hasWT, err := a.worktreeOf(o.parentBranch)
	if err != nil {
		return err
	}
	if !hasWT && (o.carry != "" || o.split != "") {
		return userErrorf("--carry/--split need %s checked out in a worktree", o.parentBranch)
	}
	if hasWT {
		dirty, err := a.vcs.Dirty(a.ctx, parentWT, false)
		if err != nil {
			return err
		}
		switch {
		case o.carry != "" && !dirty:
			return userErrorf("nothing to carry: %s has no uncommitted changes", parentWT)
		case o.carry == "" && dirty:
			return userErrorf("%s has uncommitted changes; commit or stash them first, or move them into a variant with --carry <variant>", parentWT)
		}
	}
	if o.carry != "" && !materialized {
		return userErrorf("cannot --carry into a deferred decision (its parent variant is not the default)")
	}

	baseTip := parentTip // where every variant stacks
	if o.split != "" {
		rev, err := a.vcs.ResolveCommit(a.ctx, parentWT, o.split)
		if err != nil {
			return userErrorf("--split: unknown revision %q", o.split)
		}
		if rev == parentTip {
			return userErrorf("--split %s: no commits between %s and the tip of %s", o.split, o.split, o.parentBranch)
		}
		if ok, err := gitx.IsAncestor(a.ctx, a.mainRoot, rev, parentTip); err != nil {
			return err
		} else if !ok {
			return userErrorf("--split %s is not an ancestor of %s", o.split, o.parentBranch)
		}
		if o.underRef != nil {
			pv := st.Variant(*o.underRef)
			if ok, _ := gitx.IsAncestor(a.ctx, a.mainRoot, pv.BaseTip, rev); !ok {
				return userErrorf("--split %s would cut below the base of %s", o.split, o.underRef)
			}
		}
		baseTip = rev
	}

	var carried backend.Saved
	if o.carry != "" {
		carried, err = a.vcs.SaveChanges(a.ctx, parentWT, "bothways carry into "+o.decisionID+"="+o.carry)
		if err != nil {
			return err
		}
		if carried.ID == "" {
			return userErrorf("nothing to carry: %s has no uncommitted changes", parentWT)
		}
	}

	// Branches first: they are the durable record.
	var created []string
	for i, alt := range o.alts {
		ref := state.Ref{Decision: o.decisionID, Variant: alt}
		start := baseTip
		if i == 0 && o.split != "" {
			start = parentTip
		}
		if err := a.vcs.CreateBranch(a.ctx, ref.Branch(), start); err != nil {
			for _, b := range created {
				a.vcs.DeleteBranch(a.ctx, b)
			}
			return userErrorf("creating %s: %v", ref.Branch(), err)
		}
		created = append(created, ref.Branch())
		variantState := state.Idle
		if !materialized {
			variantState = state.Deferred
		}
		decision.Variants = append(decision.Variants, &state.Variant{ID: alt, Branch: ref.Branch(), BaseTip: baseTip, State: variantState})
	}
	err = a.update(func(st *state.State) error {
		if st.Decision(o.decisionID) != nil {
			return userErrorf("decision %q was opened concurrently", o.decisionID)
		}
		if o.underRef == nil {
			st.BaseBranch = base
		}
		st.Decisions = append(st.Decisions, decision)
		return nil
	})
	if err != nil {
		for _, b := range created {
			a.vcs.DeleteBranch(a.ctx, b)
		}
		return err
	}
	if st, err = a.load(); err != nil {
		return err
	}
	if err := a.writeDescriptions(st, st.Decision(o.decisionID)); err != nil {
		return err
	}

	if o.split != "" {
		if err := a.moveBranch(o.parentBranch, parentTip, baseTip, "bothways: fork --split "+o.decisionID); err != nil {
			return err
		}
	}

	carryOK := false
	_, err = a.reconcile(func(ref state.Ref, path string) error {
		if carried.ID == "" || ref.Decision != o.decisionID || ref.Variant != o.carry {
			return nil
		}
		if err := a.vcs.ApplySaved(a.ctx, carried, path); err != nil {
			return userErrorf("applying carried changes to %s failed; they are kept in %s: %v", path, a.vcs.Vocab().SavedList, err)
		}
		carryOK = true
		return nil
	})
	if err != nil {
		return err
	}
	if carryOK {
		if err := a.vcs.DiscardChanges(a.ctx, parentWT); err != nil {
			return err
		}
	}

	if err := a.printPaths(o.decisionID); err != nil {
		return err
	}

	if o.split != "" {
		// Everything that stacked on the old tip now contains the split-off commits.
		var others []state.Ref
		st, err := a.load()
		if err != nil {
			return err
		}
		for _, d := range st.Decisions {
			if d.ID == o.decisionID {
				continue
			}
			for _, v := range d.Variants {
				others = append(others, d.Ref(v))
			}
		}
		if len(others) > 0 {
			results, err := a.sync(others)
			if err != nil {
				return err
			}
			if n := reportSync(a.errw, results, true); n > 0 {
				return partialErrorf("forked %s, but %d other variant(s) did not sync", o.decisionID, n)
			}
		}
	}
	return nil
}

// forkParent resolves which branch the new decision stacks on and returns the
// base branch name.
func (a *app) forkParent(st *state.State, o *forkOptions) (string, error) {
	if o.under != "" {
		ref, err := st.Resolve(o.under)
		if err != nil {
			return "", userErrorf("--under: %v", err)
		}
		pv := st.Variant(ref)
		if pv.State == state.Conflict {
			return "", userErrorf("--under %s: variant is in conflict; resolve it first", ref)
		}
		o.underRef = &ref
		o.parentBranch = pv.Branch
		return st.BaseBranch, nil
	}
	current, err := a.vcs.CurrentBranch(a.ctx, a.cwd)
	if err != nil {
		return "", err
	}
	inVariant := strings.HasPrefix(current, state.BranchPrefix)
	var base string
	switch {
	case len(st.Decisions) > 0:
		base = st.BaseBranch
		if current != "" && !inVariant && current != base {
			return "", userErrorf("open decisions stack on %q but %s is on %q; check out %s or finish those decisions first", base, a.cwd, current, base)
		}
	case current != "" && !inVariant:
		base = current
	case st.BaseBranch != "":
		base = st.BaseBranch
	default:
		return "", userErrorf("cannot tell the base branch: HEAD is detached; check out your working branch")
	}
	o.parentBranch = base
	return base, nil
}

func checkSimilar(st *state.State, question string, threshold float64, force bool) error {
	if question == "" || force {
		return nil
	}
	var similar []string
	for _, d := range st.Decisions {
		if s := state.Similarity(question, d.Question); d.Question != "" && s >= threshold {
			similar = append(similar, fmt.Sprintf("%s (%.2f): %q", d.ID, s, d.Question))
		}
	}
	if len(similar) == 0 {
		return nil
	}
	return userErrorf("question looks like an open decision: %s; pass --force to fork anyway", strings.Join(similar, "; "))
}

func validateAlts(alts []string) error {
	if len(alts) == 0 {
		return userErrorf("at least one --alt is required")
	}
	seen := map[string]bool{}
	for _, alt := range alts {
		if err := state.ValidateID("variant", alt); err != nil {
			return userErrorf("%v", err)
		}
		if seen[alt] {
			return userErrorf("duplicate --alt %q", alt)
		}
		seen[alt] = true
	}
	return nil
}

func newAddCmd() *cobra.Command {
	var alts []string
	cmd := &cobra.Command{
		Use:   "add <decision> --alt <c>",
		Short: "Add variants to an open decision",
		Args:  wrapArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateAlts(alts); err != nil {
				return err
			}
			a, err := openApp(cmd)
			if err != nil {
				return err
			}
			st, err := a.prepare()
			if err != nil {
				return err
			}
			d := st.Decision(args[0])
			if d == nil {
				return userErrorf("no open decision %q; open it with `bothways fork %s --alt ...`", args[0], args[0])
			}
			for _, alt := range alts {
				if d.Variant(alt) != nil {
					return userErrorf("%s=%s already exists", d.ID, alt)
				}
			}
			parent, err := a.parentBranch(st, d)
			if err != nil {
				return err
			}
			tip, err := a.tipOf(parent)
			if err != nil {
				return err
			}
			variantState := state.Idle
			if !st.Materialized(d) {
				variantState = state.Deferred
			}
			var added []*state.Variant
			for _, alt := range alts {
				ref := state.Ref{Decision: d.ID, Variant: alt}
				if err := a.vcs.CreateBranch(a.ctx, ref.Branch(), tip); err != nil {
					for _, v := range added {
						a.vcs.DeleteBranch(a.ctx, v.Branch)
					}
					return userErrorf("creating %s: %v", ref.Branch(), err)
				}
				added = append(added, &state.Variant{ID: alt, Branch: ref.Branch(), BaseTip: tip, State: variantState})
			}
			err = a.update(func(st *state.State) error {
				d := st.Decision(args[0])
				if d == nil {
					return userErrorf("decision %q was closed concurrently", args[0])
				}
				d.Variants = append(d.Variants, added...)
				return nil
			})
			if err != nil {
				return err
			}
			if st, err = a.load(); err != nil {
				return err
			}
			if err := a.writeDescriptions(st, st.Decision(args[0])); err != nil {
				return err
			}
			if _, err := a.reconcile(nil); err != nil {
				return err
			}
			return a.printPaths(args[0], alts...)
		},
	}
	cmd.Flags().StringArrayVar(&alts, "alt", nil, "variant id (repeatable)")
	return cmd
}

// printPaths prints "<decision>=<variant>\t<abs path>" per variant (only the
// listed ones when given).
func (a *app) printPaths(decisionID string, only ...string) error {
	st, err := a.load()
	if err != nil {
		return err
	}
	d := st.Decision(decisionID)
	if d == nil {
		return nil
	}
	for _, v := range d.Variants {
		if len(only) > 0 && !slices.Contains(only, v.ID) {
			continue
		}
		where := v.Worktree
		if where == "" {
			where = "(deferred)"
		}
		fmt.Fprintf(a.out, "%s\t%s\n", d.Ref(v), where)
	}
	return nil
}
