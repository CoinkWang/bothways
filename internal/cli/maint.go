package cli

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/CoinkWang/bothways/internal/backend"
	"github.com/CoinkWang/bothways/internal/config"
	"github.com/CoinkWang/bothways/internal/gitx"
	"github.com/CoinkWang/bothways/internal/state"
	"github.com/CoinkWang/bothways/internal/worktree"
)

func newGCCmd() *cobra.Command {
	var olderThan string
	var force bool
	cmd := &cobra.Command{
		Use:   "gc [--older-than 14d] [--force]",
		Short: "Delete old rejected branches and orphan worktrees, and reclaim disk down to [worktree] max_disk",
		Args:  wrapArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			age, err := parseAge(olderThan)
			if err != nil {
				return userErrorf("--older-than: %v", err)
			}
			a, err := openApp(cmd)
			if err != nil {
				return err
			}
			if !a.store.Exists() {
				return userErrorf("no state.json; run `bothways doctor` before gc so open variants are not mistaken for orphans")
			}
			st, err := a.prepare()
			if err != nil {
				return err
			}

			refs, err := gitx.ListRefs(a.ctx, a.mainRoot, "refs/heads/"+state.RejectedPrefix)
			if err != nil {
				return err
			}
			recorded := map[string]time.Time{}
			for _, r := range st.Rejected {
				recorded[r.Branch] = r.At
			}
			deleted := map[string]bool{}
			alive := map[string]bool{}
			for _, r := range refs {
				branch := strings.TrimPrefix(r.Name, "refs/heads/")
				at, ok := recorded[branch]
				if !ok {
					at = time.Unix(r.CommitterUnix, 0)
				}
				if time.Since(at) < age {
					alive[branch] = true
					continue
				}
				if err := a.vcs.DeleteBranch(a.ctx, branch); err != nil {
					return err
				}
				deleted[branch] = true
				fmt.Fprintf(a.out, "deleted branch %s\n", branch)
			}
			err = a.update(func(st *state.State) error {
				kept := st.Rejected[:0]
				for _, r := range st.Rejected {
					if alive[r.Branch] {
						kept = append(kept, r)
					}
				}
				st.Rejected = kept
				return nil
			})
			if err != nil {
				return err
			}

			root, err := a.worktreeRoot(st)
			if err != nil {
				return err
			}
			st, err = a.gcCombos(st)
			if err != nil {
				return err
			}

			// Orphan worktrees: registered under our root but not an open
			// variant. Combo worktrees are detached and belong to no variant,
			// so they have to be excluded by hand.
			combos := comboPaths(st)
			open := map[string]bool{}
			for _, d := range st.Decisions {
				for _, v := range d.Variants {
					open[v.Branch] = true
				}
			}
			wts, err := a.vcs.Workspaces(a.ctx)
			if err != nil {
				return err
			}
			for _, wt := range wts[1:] {
				if !worktree.Within(root, wt.Path) || (wt.Branch != "" && open[wt.Branch]) {
					continue
				}
				if combos[worktree.Canonical(wt.Path)] {
					continue
				}
				if strings.HasPrefix(wt.Branch, state.BranchPrefix) && !strings.HasPrefix(wt.Branch, state.RejectedPrefix) {
					if _, ok, _ := gitx.BranchTip(a.ctx, a.mainRoot, wt.Branch); ok {
						a.warnf("%s has %s checked out but it is not in state.json; run `bothways doctor`", wt.Path, wt.Branch)
						continue
					}
				}
				if err := a.removeWorktree(wt.Path); err != nil {
					return err
				}
				fmt.Fprintf(a.out, "removed orphan worktree %s\n", wt.Path)
			}
			if err := a.gcBudget(root, force); err != nil {
				return err
			}
			removeEmptyDirs(root)
			return nil
		},
	}
	cmd.Flags().StringVar(&olderThan, "older-than", "14d", "age after which rejected branches are deleted (e.g. 14d, 36h, 0s)")
	cmd.Flags().BoolVar(&force, "force", false, "reclaim worktrees that are in conflict or have uncommitted changes")
	return cmd
}

// gcCombos drops combos whose directory or variants are gone, and returns the
// state without them. A combo is derived data: nothing is lost by rebuilding it.
func (a *app) gcCombos(st *state.State) (*state.State, error) {
	dropped := false
	for _, c := range st.Combos {
		why := staleCombo(st, c)
		if why == "" {
			continue
		}
		if err := a.dropCombo(c.Name, c.Worktree); err != nil {
			return nil, err
		}
		fmt.Fprintf(a.out, "removed stale combo %s (%s)\n", c.Name, why)
		dropped = true
	}
	if !dropped {
		return st, nil
	}
	return a.load()
}

// gcBudget reclaims worktrees, cheapest to rebuild first, until the worktree
// root fits in [worktree] max_disk. Combos go before variants: a combo
// is one merge away from existing again, while a variant worktree costs a
// checkout plus setup.
func (a *app) gcBudget(root string, force bool) error {
	if a.cfg.Worktree.MaxDisk == "" {
		return nil
	}
	budget, err := config.ParseBytes(a.cfg.Worktree.MaxDisk)
	if err != nil {
		return userErrorf("[worktree] max_disk %q: %v", a.cfg.Worktree.MaxDisk, err)
	}
	st, err := a.load()
	if err != nil {
		return err
	}
	candidates, err := a.reclaimable(st, force)
	if err != nil {
		return err
	}
	paths := make([]string, len(candidates))
	for i, c := range candidates {
		paths[i] = c.path
	}
	total, sizes, touched := measure(root, paths)
	if total <= budget {
		return nil
	}
	for i := range candidates {
		candidates[i].size = sizes[candidates[i].path]
		candidates[i].touched = touched[candidates[i].path]
	}
	// Least recently touched first: the combination or variant nobody looked
	// at in a while is the one to lose.
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].combo != candidates[j].combo {
			return candidates[i].combo
		}
		return candidates[i].touched.Before(candidates[j].touched)
	})

	freed := int64(0)
	for _, c := range candidates {
		if total-freed <= budget {
			break
		}
		if err := c.remove(a); err != nil {
			return err
		}
		freed += c.size
		fmt.Fprintf(a.out, "reclaimed %s (%s)\n", c.label, formatBytes(c.size))
	}
	if freed == 0 {
		a.warnf("worktree root is %s, over the %s budget, but nothing can be reclaimed safely (pass --force to include worktrees with uncommitted changes)", formatBytes(total), formatBytes(budget))
		return nil
	}
	fmt.Fprintf(a.out, "freed %s; worktree root is %s of %s\n", formatBytes(freed), formatBytes(total-freed), formatBytes(budget))
	if total-freed > budget {
		a.warnf("worktree root is still over the %s budget; every remaining worktree belongs to an open variant", formatBytes(budget))
	}
	return nil
}

// candidate is one directory the disk budget may reclaim.
type candidate struct {
	label   string
	path    string
	combo   bool // combos are reclaimed before variant worktrees
	name    string
	ref     state.Ref
	size    int64
	touched time.Time
}

func (c candidate) remove(a *app) error {
	if c.combo {
		return a.dropCombo(c.name, c.path)
	}
	if err := a.removeWorktree(c.path); err != nil {
		return err
	}
	// Leave no path behind that no longer exists: reconcile recreates the
	// worktree on the next sync, up or doctor.
	ref := c.ref
	return a.update(func(st *state.State) error {
		if v := st.Variant(ref); v != nil {
			v.Worktree, v.State = "", state.Reclaimed
		}
		return nil
	})
}

// reclaimable lists what the budget may take: combos first, then idle variant
// worktrees. Conflicted or dirty worktrees are left alone unless force.
func (a *app) reclaimable(st *state.State, force bool) ([]candidate, error) {
	var out []candidate
	for _, c := range st.Combos {
		if !dirExists(c.Worktree) {
			continue
		}
		// The READONLY marker is untracked by design, so only tracked changes
		// count as work that would be lost here.
		if !force {
			dirty, err := a.vcs.Dirty(a.ctx, c.Worktree, false)
			if err != nil {
				return nil, err
			}
			if dirty {
				a.warnf("combo %s has uncommitted changes; keeping it (pass --force)", c.Name)
				continue
			}
		}
		out = append(out, candidate{label: "combo " + c.Name, path: c.Worktree, combo: true, name: c.Name})
	}
	for _, d := range st.Decisions {
		for _, v := range d.Variants {
			if v.Worktree == "" || !dirExists(v.Worktree) {
				continue
			}
			if !force {
				if v.State == state.Conflict {
					continue // an unresolved conflict only lives in the worktree
				}
				dirty, err := a.vcs.Dirty(a.ctx, v.Worktree, true)
				if err != nil {
					return nil, err
				}
				if dirty {
					continue
				}
			}
			out = append(out, candidate{label: "worktree " + d.Ref(v).String(), path: v.Worktree, ref: d.Ref(v)})
		}
	}
	return out, nil
}

// measure walks root once, returning its total size plus the size and newest
// modification time of each of the given directories.
func measure(root string, dirs []string) (total int64, size map[string]int64, touched map[string]time.Time) {
	size, touched = map[string]int64{}, map[string]time.Time{}
	owners := make(map[string]string, len(dirs))
	for _, d := range dirs {
		owners[worktree.Canonical(d)+string(filepath.Separator)] = d
	}
	filepath.WalkDir(worktree.Canonical(root), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil // unreadable entries simply do not count
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		total += info.Size()
		for prefix, owner := range owners {
			if strings.HasPrefix(p, prefix) {
				size[owner] += info.Size()
				if info.ModTime().After(touched[owner]) {
					touched[owner] = info.ModTime()
				}
				break
			}
		}
		return nil
	})
	return total, size, touched
}

// formatBytes renders a size the way max_disk is written ("20GB").
func formatBytes(n int64) string {
	for _, u := range []struct {
		suffix string
		mult   float64
	}{{"TB", 1e12}, {"GB", 1e9}, {"MB", 1e6}, {"KB", 1e3}} {
		if float64(n) >= u.mult {
			return fmt.Sprintf("%.1f%s", float64(n)/u.mult, u.suffix)
		}
	}
	return fmt.Sprintf("%dB", n)
}

// parseAge accepts Go durations plus a day suffix ("14d").
func parseAge(s string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.ParseFloat(days, 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("invalid age %q", s)
		}
		return time.Duration(n * float64(24*time.Hour)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("invalid age %q", s)
	}
	return d, nil
}

// removeEmptyDirs removes empty decision directories directly under root.
func removeEmptyDirs(root string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			os.Remove(filepath.Join(root, e.Name())) // fails unless empty
		}
	}
	os.Remove(root)
}

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Rebuild state.json from bothways/* branches and repair inconsistencies",
		Args:  wrapArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := openApp(cmd)
			if err != nil {
				return err
			}
			return runDoctor(a)
		},
	}
}

func runDoctor(a *app) error {
	if err := a.ensureInit(); err != nil {
		return err
	}
	if err := a.vcs.PruneWorkspaces(a.ctx); err != nil {
		return err
	}
	report := func(format string, args ...any) { fmt.Fprintf(a.out, format+"\n", args...) }
	old, err := a.store.Load()
	switch {
	case err != nil:
		report("state.json is unreadable (%v); rebuilding from branches", err)
		old = state.New()
	case !a.store.Exists():
		report("state.json is missing; rebuilding from branches")
	}

	refs, err := gitx.ListRefs(a.ctx, a.mainRoot, "refs/heads/"+state.BranchPrefix)
	if err != nil {
		return err
	}
	wts, err := a.vcs.Workspaces(a.ctx)
	if err != nil {
		return err
	}
	checkedOut := map[string]backend.Workspace{}
	for _, wt := range wts {
		if wt.Branch != "" {
			checkedOut[wt.Branch] = wt
		}
	}

	fresh := state.New()
	fresh.BaseBranch = old.BaseBranch
	fresh.Picked = old.Picked
	oldRejected := map[string]state.Rejected{}
	for _, r := range old.Rejected {
		oldRejected[r.Branch] = r
	}
	descs := map[string]map[string]string{} // decision id -> description fields
	for _, r := range refs {
		branch := strings.TrimPrefix(r.Name, "refs/heads/")
		if rest, ok := strings.CutPrefix(branch, state.RejectedPrefix); ok {
			rej, known := oldRejected[branch]
			if !known {
				d, v, _ := strings.Cut(rest, "/")
				rej = state.Rejected{Decision: d, Variant: v, Branch: branch, At: time.Unix(r.CommitterUnix, 0).UTC()}
			}
			fresh.Rejected = append(fresh.Rejected, rej)
			continue
		}
		parts := strings.Split(strings.TrimPrefix(branch, state.BranchPrefix), "/")
		if len(parts) != 2 || state.ValidateID("decision", parts[0]) != nil || state.ValidateID("variant", parts[1]) != nil {
			report("ignoring branch %s: not bothways/<decision>/<variant>", branch)
			continue
		}
		ref := state.Ref{Decision: parts[0], Variant: parts[1]}
		d := fresh.Decision(ref.Decision)
		if d == nil {
			d = &state.Decision{ID: ref.Decision, CreatedAt: now()}
			if od := old.Decision(ref.Decision); od != nil {
				d.Question, d.Default, d.CreatedAt = od.Question, od.Default, od.CreatedAt
				if od.Under != nil {
					u := *od.Under
					d.Under = &u
				}
			} else {
				report("adopting decision %s from branches", ref.Decision)
			}
			fresh.Decisions = append(fresh.Decisions, d)
		}
		desc, err := a.vcs.Description(a.ctx, branch)
		if err != nil {
			return err
		}
		fields := parseDescription(desc)
		if descs[d.ID] == nil {
			descs[d.ID] = fields
		}
		v := &state.Variant{ID: ref.Variant, Branch: branch, State: state.Idle}
		if ov := old.Variant(ref); ov != nil {
			*v = *ov
		} else if old.Decision(ref.Decision) != nil {
			report("adopting variant %s from branches", ref)
		}
		v.Worktree = ""
		if wt, ok := checkedOut[branch]; ok {
			v.Worktree = wt.Path
		}
		d.Variants = append(d.Variants, v)
	}
	for _, od := range old.Decisions {
		for _, ov := range od.Variants {
			if fresh.Variant(od.Ref(ov)) == nil {
				report("forgetting %s: branch %s no longer exists", od.Ref(ov), ov.Branch)
			}
		}
	}

	// Fill what only the branch descriptions know.
	for _, d := range fresh.Decisions {
		fields := descs[d.ID]
		if fresh.BaseBranch == "" && fields["base"] != "" {
			fresh.BaseBranch = fields["base"]
		}
		if d.Question == "" {
			d.Question = fields["question"]
		}
		if d.Default == "" {
			d.Default = fields["default"]
		}
		if d.Under == nil && old.Decision(d.ID) == nil && fields["under"] != "" {
			if u, err := state.ParseRef(fields["under"]); err == nil {
				d.Under = &u
			}
		}
		if d.Variant(d.Default) == nil {
			sort.Slice(d.Variants, func(i, j int) bool { return d.Variants[i].ID < d.Variants[j].ID })
			if d.Default != "" {
				report("%s: default %s is gone; using %s", d.ID, d.Default, d.Variants[0].ID)
			}
			d.Default = d.Variants[0].ID
		}
	}
	if fresh.BaseBranch == "" {
		if cur, _ := a.vcs.CurrentBranch(a.ctx, a.mainRoot); cur != "" && !strings.HasPrefix(cur, state.BranchPrefix) {
			fresh.BaseBranch = cur
			report("assuming base branch %s", cur)
		}
	}
	for _, d := range fresh.Decisions {
		if d.Under != nil && fresh.Variant(*d.Under) == nil {
			report("%s: parent %s is gone; treating it as a top-level decision", d.ID, d.Under)
			d.Under = nil
		}
	}

	// Recompute base tips and conflict flags from git.
	for _, d := range fresh.Ordered() {
		parent, err := a.parentBranch(fresh, d)
		if err != nil {
			return err
		}
		parentTip, ok, err := gitx.BranchTip(a.ctx, a.mainRoot, parent)
		if err != nil {
			return err
		}
		for _, v := range d.Variants {
			tip, _, err := gitx.BranchTip(a.ctx, a.mainRoot, v.Branch)
			if err != nil {
				return err
			}
			// A recorded base tip still in the branch's history beats merge-base,
			// which is wrong once the base itself was rewritten.
			stacked := false
			if v.BaseTip != "" {
				stacked, _ = gitx.IsAncestor(a.ctx, a.mainRoot, v.BaseTip, tip)
			}
			if !stacked && ok {
				mb, err := gitx.MergeBase(a.ctx, a.mainRoot, parentTip, tip)
				if err != nil {
					return err
				}
				if v.BaseTip != mb {
					report("%s: base_tip set to %s (merge-base with %s)", d.Ref(v), short(mb), parent)
				}
				v.BaseTip = mb
			}
			busy := false
			if v.Worktree != "" {
				busy, _ = a.vcs.InConflict(a.ctx, v.Worktree)
			}
			switch {
			case busy:
				v.State = state.Conflict
			case v.State == state.Conflict && v.Worktree != "":
				v.State, v.SyncOnto = state.Idle, ""
			case v.State == "":
				v.State = state.Idle
			}
		}
	}

	// Combos are not variants, so the branch scan above cannot rebuild them;
	// carry the records over and forget the ones that no longer stand up.
	for _, c := range old.Combos {
		if why := staleCombo(fresh, c); why != "" {
			report("forgetting combo %s: %s", c.Name, why)
			continue
		}
		fresh.Combos = append(fresh.Combos, c)
	}

	if err := a.store.Replace(fresh); err != nil {
		return envErrorf("writing state: %v", err)
	}
	for _, d := range fresh.Decisions {
		if err := a.writeDescriptions(fresh, d); err != nil {
			return err
		}
	}
	created, err := a.reconcile(nil)
	for _, ref := range created {
		st, _ := a.load()
		if v := st.Variant(ref); v != nil {
			report("created worktree for %s: %s", ref, v.Worktree)
		}
	}
	if err != nil {
		return err
	}
	report("ok: %d open decision(s), base %s", len(fresh.Decisions), orDash(fresh.BaseBranch))
	return nil
}

func newDiffCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "diff <decision>=<a> [<decision>=<b>] [-- <git diff args>]",
		Short: "git diff a variant against its base, or two variants against each other",
		Args: wrapArgs(func(cmd *cobra.Command, args []string) error {
			n := len(args)
			if dash := cmd.ArgsLenAtDash(); dash >= 0 {
				n = dash
			}
			if n < 1 || n > 2 {
				return fmt.Errorf("expected one or two variants, got %d", n)
			}
			return nil
		}),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openApp(cmd)
			if err != nil {
				return err
			}
			st, err := a.load()
			if err != nil {
				return err
			}
			refArgs, extra := args, []string(nil)
			if dash := cmd.ArgsLenAtDash(); dash >= 0 {
				refArgs, extra = args[:dash], args[dash:]
			}
			var branches []string
			for _, arg := range refArgs {
				ref, err := st.Resolve(arg)
				if err != nil {
					return userErrorf("%v", err)
				}
				branches = append(branches, st.Variant(ref).Branch)
			}
			if len(branches) == 1 {
				ref, _ := st.Resolve(refArgs[0])
				parent, err := a.parentBranch(st, st.Decision(ref.Decision))
				if err != nil {
					return err
				}
				branches = []string{parent, branches[0]}
			}
			rangeArg := "refs/heads/" + branches[0] + "..refs/heads/" + branches[1]
			if asJSON {
				return a.diffJSON(branches, rangeArg, extra)
			}
			env := []string{}
			if os.Getenv("LESSCHARSET") == "" {
				env = append(env, "LESSCHARSET=utf-8") // LC_ALL=C would make less escape UTF-8
			}
			if pager := os.Getenv("BOTHWAYS_PAGER"); pager != "" {
				env = append(env, "GIT_PAGER="+pager)
			}
			gitArgs := []string{"diff"}
			if os.Getenv("NO_COLOR") != "" {
				gitArgs = append(gitArgs, "--no-color")
			}
			gitArgs = append(append(gitArgs, rangeArg), extra...)
			_, err = gitx.Exec(a.ctx, gitx.Cmd{Dir: a.mainRoot, Args: gitArgs, Env: env, Stdin: os.Stdin, Stdout: a.out, Stderr: a.errw})
			if err != nil {
				return userErrorf("git diff failed")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print per-file statistics as JSON (git diff --numstat -z)")
	return cmd
}

type diffFile struct {
	Path    string `json:"path"`
	OldPath string `json:"old_path,omitempty"`
	Added   *int   `json:"added"` // null for binary files
	Deleted *int   `json:"deleted"`
	Binary  bool   `json:"binary"`
}

func (a *app) diffJSON(branches []string, rangeArg string, extra []string) error {
	out, err := gitx.Run(a.ctx, a.mainRoot, append([]string{"diff", "--numstat", "-z", rangeArg}, extra...)...)
	if err != nil {
		return userErrorf("%v", err)
	}
	files := []diffFile{}
	fields := strings.Split(out, "\x00")
	for i := 0; i < len(fields); i++ {
		if fields[i] == "" {
			continue
		}
		parts := strings.SplitN(fields[i], "\t", 3)
		if len(parts) != 3 {
			continue
		}
		f := diffFile{Path: parts[2]}
		if parts[0] == "-" && parts[1] == "-" {
			f.Binary = true
		} else {
			added, _ := strconv.Atoi(parts[0])
			deleted, _ := strconv.Atoi(parts[1])
			f.Added, f.Deleted = &added, &deleted
		}
		if f.Path == "" && i+2 < len(fields) { // rename: "\0old\0new"
			f.OldPath, f.Path = fields[i+1], fields[i+2]
			i += 2
		}
		files = append(files, f)
	}
	return writeJSON(a.out, map[string]any{
		"schema_version": state.SchemaVersion,
		"from":           branches[0],
		"to":             branches[1],
		"files":          files,
	})
}

func newNoteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   `note <decision>=<variant> "text"`,
		Short: "Attach a note (CI result, reviewer remark, ...) shown in status",
		Args:  wrapArgs(cobra.MinimumNArgs(2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openApp(cmd)
			if err != nil {
				return err
			}
			text := strings.Join(strings.Fields(strings.Join(args[1:], " ")), " ")
			if text == "" {
				return userErrorf("note text is empty")
			}
			var ref state.Ref
			err = a.update(func(st *state.State) error {
				r, err := st.Resolve(args[0])
				if err != nil {
					return userErrorf("%v", err)
				}
				ref = r
				v := st.Variant(r)
				v.Notes = append(v.Notes, text)
				return nil
			})
			if err != nil {
				return err
			}
			a.infof("noted %s", ref)
			return nil
		},
	}
}
