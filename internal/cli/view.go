package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"github.com/CoinkWang/bothways/internal/config"
	"github.com/CoinkWang/bothways/internal/gitx"
	"github.com/CoinkWang/bothways/internal/preview"
	"github.com/CoinkWang/bothways/internal/state"
)

// statusView is the JSON contract shared by status and tree: the state.json
// structure plus computed fields.
type statusView struct {
	SchemaVersion int              `json:"schema_version"`
	BaseBranch    string           `json:"base_branch"`
	BaseTip       string           `json:"base_tip"`
	Decisions     []*decisionView  `json:"decisions"`
	Picked        []state.Picked   `json:"picked"`
	Rejected      []state.Rejected `json:"rejected"`
	Combos        []state.Combo    `json:"combos"`
	Hooks         *hooksView       `json:"hooks"`
}

type decisionView struct {
	state.Decision
	ParentBranch string         `json:"parent_branch"`
	Deferred     bool           `json:"deferred"`
	Variants     []*variantView `json:"variants"`
}

type variantView struct {
	state.Variant
	Name       string `json:"name"`
	Default    bool   `json:"default"`
	Tip        string `json:"tip"`
	Ahead      int    `json:"ahead"`
	BehindBase int    `json:"behind_base"`
	Deferred   bool   `json:"deferred"`
}

// buildView computes the view without modifying state.json.
func (a *app) buildView() (*statusView, *state.State, error) {
	st, err := a.load()
	if err != nil {
		return nil, nil, err
	}
	view := &statusView{
		SchemaVersion: state.SchemaVersion,
		BaseBranch:    st.BaseBranch,
		Decisions:     []*decisionView{},
		Picked:        st.Picked,
		Rejected:      st.Rejected,
		Combos:        st.Combos,
		Hooks:         a.hooksView(),
	}
	if st.BaseBranch != "" {
		if tip, ok, err := gitx.BranchTip(a.ctx, a.mainRoot, st.BaseBranch); err != nil {
			return nil, nil, err
		} else if ok {
			view.BaseTip = tip
		}
	}
	var g errgroup.Group
	g.SetLimit(8)
	for _, d := range st.Ordered() {
		dv := &decisionView{Decision: *d, Deferred: !st.Materialized(d), Variants: []*variantView{}}
		dv.Decision.Variants = nil
		dv.ParentBranch, _ = a.parentBranch(st, d)
		view.Decisions = append(view.Decisions, dv)
		for _, v := range d.Variants {
			vv := &variantView{Variant: *v, Name: d.Ref(v).String(), Default: v.ID == d.Default}
			dv.Variants = append(dv.Variants, vv)
			g.Go(func() error { return a.fillVariant(vv, dv) })
		}
	}
	if err := g.Wait(); err != nil {
		return nil, nil, err
	}
	return view, st, nil
}

func (a *app) fillVariant(vv *variantView, dv *decisionView) error {
	switch {
	case vv.State == state.Conflict:
		if baseTip, resolved := a.conflictResolved(&vv.Variant); resolved {
			vv.State, vv.SyncOnto = state.Idle, ""
			if baseTip != "" {
				vv.BaseTip = baseTip
			}
		}
	case vv.Worktree != "" && !dirExists(vv.Worktree):
		vv.State = state.Missing
	case vv.Worktree == "" && vv.State == state.Reclaimed && !dv.Deferred:
		// Expected, not an error: gc took the cache, reconcile brings it back.
	case vv.Worktree == "" && dv.Deferred:
		vv.State = state.Deferred
	case vv.Worktree == "":
		vv.State = state.Missing
	default:
		vv.State = state.Idle
	}
	vv.Deferred = vv.State == state.Deferred
	vv.Preview = previewOf(&vv.Variant, a.cfg.Preview.Mode())
	if vv.Preview.State == state.PreviewDown {
		vv.Slot = 0
	}
	tip, ok, err := gitx.BranchTip(a.ctx, a.mainRoot, vv.Branch)
	if err != nil {
		return err
	}
	if !ok {
		vv.State = state.Missing
		return nil
	}
	vv.Tip = tip
	if vv.BaseTip == "" {
		return nil
	}
	if vv.Ahead, err = gitx.Count(a.ctx, a.mainRoot, vv.BaseTip, tip); err != nil {
		return err
	}
	if dv.ParentBranch != "" {
		if _, ok, _ := gitx.BranchTip(a.ctx, a.mainRoot, dv.ParentBranch); ok {
			if vv.BehindBase, err = gitx.Count(a.ctx, a.mainRoot, vv.BaseTip, "refs/heads/"+dv.ParentBranch); err != nil {
				return err
			}
		}
	}
	return nil
}

// previewOf returns a copy of v's preview as it is right now, so that every
// variant in the JSON carries preview.state. A recorded pid is only a
// claim: bothways exiting never kills a preview, but the process may have died
// since. Nothing is written back; the next write command does that.
func previewOf(v *state.Variant, mode string) *state.Preview {
	p := state.Preview{Mode: mode, State: state.PreviewDown}
	if v.Preview != nil {
		p = *v.Preview
	}
	if p.Mode == config.ModeStart && p.State != state.PreviewDown && !preview.Alive(p.PID, p.StartedAt) {
		p = state.Preview{Mode: p.Mode, Log: p.Log, State: state.PreviewDown}
	}
	return &p
}

// displayState folds the preview into the single STATE column of status:
// a variant that is merely idle reads as its preview's state.
func displayState(v *variantView) string {
	if v.State == state.Idle && v.Preview != nil && v.Preview.State != state.PreviewDown {
		return v.Preview.State
	}
	return v.State
}

func previewURL(v *variantView) string {
	if v.Preview == nil {
		return ""
	}
	return v.Preview.URL
}

func newStatusCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show every open variant: state, how far behind its base, worktree, notes",
		Args:  wrapArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := openApp(cmd)
			if err != nil {
				return err
			}
			view, _, err := a.buildView()
			if err != nil {
				return err
			}
			if asJSON {
				return writeJSON(a.out, view)
			}
			printStatus(a.out, view)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func printStatus(w io.Writer, view *statusView) {
	if len(view.Decisions) == 0 {
		fmt.Fprintf(w, "no open decisions (base: %s)\n", orDash(view.BaseBranch))
		return
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "DECISION\tVARIANT\tSTATE\tURL\tBASE\tWORKTREE\tNOTES")
	for _, d := range view.Decisions {
		for _, v := range d.Variants {
			name := v.ID
			if v.Default {
				name += " *"
			}
			base := "="
			switch {
			case v.Tip == "" || v.BaseTip == "":
				base = "?"
			case v.BehindBase > 0:
				base = fmt.Sprintf("-%d", v.BehindBase)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", d.ID, name, displayState(v), orDash(previewURL(v)), base, orDash(v.Worktree), strings.Join(v.Notes, "; "))
		}
	}
	tw.Flush()
	if len(view.Combos) == 0 {
		return
	}
	fmt.Fprintln(w)
	tw = tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "COMBO (read-only)\tVARIANTS\tWORKTREE")
	for _, c := range view.Combos {
		names := make([]string, len(c.Variants))
		for i, r := range c.Variants {
			names[i] = r.String()
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", c.Name, strings.Join(names, " "), c.Worktree)
	}
	tw.Flush()
}

func newTreeCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "tree",
		Short: "Show open decisions as a tree rooted at the base branch",
		Args:  wrapArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := openApp(cmd)
			if err != nil {
				return err
			}
			view, _, err := a.buildView()
			if err != nil {
				return err
			}
			tree := buildTree(view)
			if asJSON {
				return writeJSON(a.out, tree)
			}
			fmt.Fprintf(a.out, "%s %s\n", orDash(tree.BaseBranch), short(tree.BaseTip))
			printTreeDecisions(a.out, tree.Decisions, "")
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

type treeView struct {
	SchemaVersion int             `json:"schema_version"`
	BaseBranch    string          `json:"base_branch"`
	BaseTip       string          `json:"base_tip"`
	Decisions     []*treeDecision `json:"decisions"`
}

type treeDecision struct {
	ID       string         `json:"id"`
	Question string         `json:"question"`
	Default  string         `json:"default"`
	Deferred bool           `json:"deferred"`
	Variants []*treeVariant `json:"variants"`
}

type treeVariant struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Default   bool            `json:"default"`
	State     string          `json:"state"`
	Branch    string          `json:"branch"`
	Worktree  string          `json:"worktree"`
	URL       string          `json:"url,omitempty"`
	Preview   *state.Preview  `json:"preview"`
	Decisions []*treeDecision `json:"decisions"`
}

func buildTree(view *statusView) *treeView {
	children := map[state.Ref][]*decisionView{}
	var roots []*decisionView
	for _, d := range view.Decisions {
		if d.Under == nil {
			roots = append(roots, d)
		} else {
			children[*d.Under] = append(children[*d.Under], d)
		}
	}
	var convert func([]*decisionView) []*treeDecision
	convert = func(list []*decisionView) []*treeDecision {
		out := []*treeDecision{}
		for _, d := range list {
			td := &treeDecision{ID: d.ID, Question: d.Question, Default: d.Default, Deferred: d.Deferred, Variants: []*treeVariant{}}
			for _, v := range d.Variants {
				td.Variants = append(td.Variants, &treeVariant{
					ID: v.ID, Name: v.Name, Default: v.Default, State: v.State, Branch: v.Branch, Worktree: v.Worktree,
					URL: previewURL(v), Preview: v.Preview,
					Decisions: convert(children[state.Ref{Decision: d.ID, Variant: v.ID}]),
				})
			}
			out = append(out, td)
		}
		return out
	}
	return &treeView{SchemaVersion: state.SchemaVersion, BaseBranch: view.BaseBranch, BaseTip: view.BaseTip, Decisions: convert(roots)}
}

func printTreeDecisions(w io.Writer, list []*treeDecision, prefix string) {
	for i, d := range list {
		branch, next := "├── ", "│   "
		if i == len(list)-1 {
			branch, next = "└── ", "    "
		}
		label := d.ID
		if d.Question != "" {
			label += ": " + d.Question
		}
		fmt.Fprintf(w, "%s%s%s\n", prefix, branch, label)
		for j, v := range d.Variants {
			vBranch, vNext := "├── ", "│   "
			if j == len(d.Variants)-1 {
				vBranch, vNext = "└── ", "    "
			}
			label := v.ID
			if v.Default {
				label += " *"
			}
			shown := v.State
			if v.State == state.Idle && v.Preview != nil && v.Preview.State != state.PreviewDown {
				shown = v.Preview.State
			}
			if shown != state.Idle {
				label += " [" + shown + "]"
			}
			if v.URL != "" {
				label += " " + v.URL
			}
			fmt.Fprintf(w, "%s%s%s%s\n", prefix, next, vBranch, label)
			printTreeDecisions(w, v.Decisions, prefix+next+vNext)
		}
	}
}

func newPathCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "path <decision>=<variant>",
		Short: "Print the absolute worktree path of a variant",
		Args:  wrapArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openApp(cmd)
			if err != nil {
				return err
			}
			st, err := a.load()
			if err != nil {
				return err
			}
			ref, err := st.Resolve(args[0])
			if err != nil {
				return userErrorf("%v", err)
			}
			v := st.Variant(ref)
			if v.Worktree == "" && v.State == state.Reclaimed {
				return userErrorf("worktree of %s was reclaimed by gc to stay under max_disk; run `bothways sync` to rebuild it", ref)
			}
			if v.Worktree == "" {
				return userErrorf("%s is deferred and has no worktree (its parent variant is not the default)", ref)
			}
			if !dirExists(v.Worktree) {
				return userErrorf("worktree of %s is missing (%s); run `bothways doctor`", ref, v.Worktree)
			}
			if asJSON {
				return writeJSON(a.out, map[string]string{
					"name": ref.String(), "decision": ref.Decision, "variant": ref.Variant,
					"branch": v.Branch, "path": v.Worktree,
				})
			}
			fmt.Fprintln(a.out, v.Worktree)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}
