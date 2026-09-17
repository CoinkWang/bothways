package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/CoinkWang/bothways/internal/backend"
	"github.com/CoinkWang/bothways/internal/gitx"
	"github.com/CoinkWang/bothways/internal/state"
	"github.com/CoinkWang/bothways/internal/worktree"
)

// comboDir holds combo worktrees, next to the decision directories under the
// worktree root. Decision ids never collide with it: "combo"
// would be a decision id, but a decision directory holds variant directories,
// so a name clash is impossible without a variant named like a combo.
const comboDir = "combo"

// readonlyMarker is written into every combo worktree. A variant branch is the
// base plus only its own commits; a commit made in a
// merge of several variants belongs to none of them, so the directory is only
// ever read and bothways reclaims it without asking.
const readonlyMarker = "READONLY"

const readonlyText = `bothways combo worktree: a read-only merge of several variants.

Do not commit here. A commit in this directory could not be attributed to any
single variant, and bothways reclaims the directory without asking (` + "`bothways gc`" + `).
Rebuild it after the variants moved on with:

    bothways combo %s
`

func newComboCmd() *cobra.Command {
	var name string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "combo <decision>=<variant> <decision>=<variant> [...]",
		Short: "Create a read-only worktree combining variants",
		Long: `combo merges the tips of several variants on top of the current base in a
detached worktree under <worktree root>/combo/<name>, so you can look at a
combination that no single variant shows ("round frame plus red label").

The worktree is read-only: it carries a READONLY marker and nothing may be
committed in it, because a commit there could not be attributed to any single
variant. Running combo again on the same variants rebuilds it against the
current tips. gc reclaims combos first.`,
		Args: wrapArgs(cobra.MinimumNArgs(2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCombo(cmd, args, name, asJSON)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "directory name under <worktree root>/combo (default: the variant names joined with '+')")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func runCombo(cmd *cobra.Command, args []string, name string, asJSON bool) error {
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
	refs, err := comboRefs(st, args)
	if err != nil {
		return err
	}
	if name == "" {
		name = comboName(refs)
	} else if err := state.ValidateID("combo", name); err != nil {
		return userErrorf("--name: %v", err)
	}
	if st.BaseBranch == "" {
		return userErrorf("no base branch recorded; run `bothways doctor`")
	}
	baseTip, err := a.tipOf(st.BaseBranch)
	if err != nil {
		return err
	}
	if old := st.Combo(name); old != nil && !sameRefs(old.Variants, refs) {
		return userErrorf("combo %q already combines %s; pass --name or run `bothways gc`", name, refNames(old.Variants))
	}
	root, err := a.worktreeRoot(st)
	if err != nil {
		return err
	}
	path := filepath.Join(root, comboDir, name)

	// Rebuilding is the point of re-running combo: the variants moved on and
	// the old merge shows stale tips.
	if err := a.dropCombo(name, path); err != nil {
		return err
	}
	c, err := a.buildCombo(st, name, path, baseTip, refs)
	if err != nil {
		return err
	}
	err = a.update(func(st *state.State) error {
		st.RemoveCombo(name)
		st.Combos = append(st.Combos, *c)
		return nil
	})
	if err != nil {
		return err
	}
	if asJSON {
		return writeJSON(a.out, map[string]any{
			"schema_version": state.SchemaVersion,
			"name":           c.Name,
			"path":           c.Worktree,
			"variants":       c.Variants,
			"base_branch":    st.BaseBranch,
			"base_tip":       c.BaseTip,
			"commit":         c.Commit,
			"read_only":      true,
		})
	}
	a.infof("combo %s: read-only merge of %s (do not commit here)", name, refNames(refs))
	fmt.Fprintf(a.out, "%s\t%s\n", name, path)
	return nil
}

// comboRefs resolves the arguments. Two variants of one decision are
// alternatives to the same question: combining them has no meaning.
func comboRefs(st *state.State, args []string) ([]state.Ref, error) {
	var refs []state.Ref
	seen := map[string]state.Ref{}
	for _, arg := range args {
		ref, err := st.Resolve(arg)
		if err != nil {
			return nil, userErrorf("%v", err)
		}
		if other, ok := seen[ref.Decision]; ok {
			if other == ref {
				return nil, userErrorf("%s is listed twice", ref)
			}
			return nil, userErrorf("%s and %s are alternatives of the same decision; a combo cannot contain both", other, ref)
		}
		seen[ref.Decision] = ref
		refs = append(refs, ref)
	}
	return refs, nil
}

// comboName joins the variant names: shape-circle+label-color-red.
func comboName(refs []state.Ref) string {
	parts := make([]string, len(refs))
	for i, r := range refs {
		parts[i] = r.Name()
	}
	return strings.Join(parts, "+")
}

func refNames(refs []state.Ref) string {
	parts := make([]string, len(refs))
	for i, r := range refs {
		parts[i] = r.String()
	}
	return strings.Join(parts, ", ")
}

func sameRefs(a, b []state.Ref) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// buildCombo checks out the base detached and merges every variant tip into
// it. A conflict (or any other failure) leaves nothing behind.
func (a *app) buildCombo(st *state.State, name, path, baseTip string, refs []state.Ref) (*state.Combo, error) {
	if entries, err := os.ReadDir(path); err == nil && len(entries) > 0 {
		return nil, userErrorf("cannot build combo %s: %s already exists and is not empty", name, path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, envErrorf("creating %s: %v", filepath.Dir(path), err)
	}
	if err := a.vcs.AddDetachedWorkspace(a.ctx, path, baseTip); err != nil {
		return nil, userErrorf("creating combo worktree %s: %v", path, err)
	}
	discard := func() {
		a.vcs.RemoveWorkspace(a.ctx, path)
		os.RemoveAll(path)
	}

	var merged []state.Ref
	for _, ref := range refs {
		tip, err := a.tipOf(st.Variant(ref).Branch)
		if err != nil {
			discard()
			return nil, err
		}
		msg := fmt.Sprintf("bothways combo %s: merge %s", name, ref)
		mergeErr := a.vcs.Merge(a.ctx, path, tip, msg)
		if mergeErr == nil {
			merged = append(merged, ref)
			continue
		}
		var files []string
		var conflict *backend.ConflictError
		if errors.As(mergeErr, &conflict) {
			files = conflict.Files
		}
		discard()
		if len(files) == 0 {
			return nil, partialErrorf("combo %s: merging %s failed: %v", name, ref, mergeErr)
		}
		with := a.conflictPartner(st, baseTip, merged, files)
		return nil, partialErrorf("%s and %s conflict in %s; combine other variants or hoist the shared change to %s",
			with, ref, strings.Join(files, ", "), st.BaseBranch)
	}

	head, err := a.vcs.Head(a.ctx, path)
	if err != nil {
		discard()
		return nil, err
	}
	marker := fmt.Sprintf(readonlyText, strings.Join(refStrings(refs), " "))
	// Not mode 0444: a read-only file is what Windows refuses to delete, and
	// this directory has to stay cheap to throw away.
	if err := os.WriteFile(filepath.Join(path, readonlyMarker), []byte(marker), 0o644); err != nil {
		discard()
		return nil, envErrorf("writing %s: %v", readonlyMarker, err)
	}
	return &state.Combo{
		Name:      name,
		Variants:  refs,
		Worktree:  path,
		BaseTip:   baseTip,
		Commit:    head,
		CreatedAt: now(),
	}, nil
}

// conflictPartner names the variant already in the combo that changed the
// same files as the one that just failed to merge; with none, the base itself
// is the other side.
func (a *app) conflictPartner(st *state.State, baseTip string, merged []state.Ref, files []string) string {
	conflicted := map[string]bool{}
	for _, f := range files {
		conflicted[f] = true
	}
	for _, ref := range merged {
		v := st.Variant(ref)
		if v == nil {
			continue
		}
		out, err := gitx.Run(a.ctx, a.mainRoot, "diff", "--name-only", baseTip, "refs/heads/"+v.Branch)
		if err != nil {
			continue
		}
		for _, f := range strings.Split(out, "\n") {
			if conflicted[f] {
				return ref.String()
			}
		}
	}
	if len(merged) > 0 {
		return merged[len(merged)-1].String()
	}
	return st.BaseBranch
}

func refStrings(refs []state.Ref) []string {
	out := make([]string, len(refs))
	for i, r := range refs {
		out[i] = r.String()
	}
	return out
}

// dropCombo removes a combo worktree and forgets it. The directory is derived
// data, so a leftover directory is removed even when git does not know it.
func (a *app) dropCombo(name, path string) error {
	if dirExists(path) {
		if err := a.vcs.RemoveWorkspace(a.ctx, path); err != nil {
			os.RemoveAll(path)
		}
		os.RemoveAll(path)
	}
	return a.update(func(st *state.State) error {
		st.RemoveCombo(name)
		return nil
	})
}

// comboPaths is the set of canonical combo directories gc must not mistake
// for orphan worktrees.
func comboPaths(st *state.State) map[string]bool {
	paths := map[string]bool{}
	for _, c := range st.Combos {
		paths[worktree.Canonical(c.Worktree)] = true
	}
	return paths
}

// staleCombo says why a combo can no longer be shown, or "" when it is live.
func staleCombo(st *state.State, c state.Combo) string {
	if !dirExists(c.Worktree) {
		return "worktree is gone"
	}
	for _, ref := range c.Variants {
		if st.Variant(ref) == nil {
			return ref.String() + " is no longer open"
		}
	}
	return ""
}
