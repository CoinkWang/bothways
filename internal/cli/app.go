package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/CoinkWang/bothways/internal/backend"
	gitbackend "github.com/CoinkWang/bothways/internal/backend/git"
	"github.com/CoinkWang/bothways/internal/config"
	"github.com/CoinkWang/bothways/internal/gitx"
	"github.com/CoinkWang/bothways/internal/state"
	"github.com/CoinkWang/bothways/internal/worktree"
)

// app is the per-invocation repository context. Whichever worktree the
// command runs in, it resolves to the same shared git dir and state file.
type app struct {
	ctx       context.Context
	out, errw io.Writer
	cwd       string
	commonDir string
	mainRoot  string // main worktree; git -C target for repository-wide reads
	vcs       backend.Backend
	store     *state.Store
	cfg       *config.Config
	// skipDirty makes sync leave variant worktrees with uncommitted or
	// untracked changes alone instead of stashing around the rebase.
	skipDirty bool
	// setupSkipWarned keeps an untrusted config to one warning per command.
	setupSkipWarned bool
}

func openApp(cmd *cobra.Command) (*app, error) {
	ctx := cmd.Context()
	cwd, err := os.Getwd()
	if err != nil {
		return nil, envErrorf("cannot determine working directory: %v", err)
	}
	v, err := gitx.Version(ctx, cwd)
	if err != nil {
		return nil, envErrorf("running git: %v", err)
	}
	if !gitx.AtLeast(v, gitx.MinVersion) {
		return nil, envErrorf("git %d.%d.%d is too old; bothways needs git >= %d.%d", v[0], v[1], v[2], gitx.MinVersion[0], gitx.MinVersion[1])
	}
	common, err := gitx.CommonDir(ctx, cwd)
	if err != nil {
		return nil, envErrorf("not a git repository: %s", cwd)
	}
	wts, err := gitx.Worktrees(ctx, cwd)
	if err != nil {
		return nil, envErrorf("listing worktrees: %v", err)
	}
	if len(wts) == 0 || wts[0].Bare {
		return nil, envErrorf("bare repositories are not supported")
	}
	a := &app{
		ctx:       ctx,
		out:       cmd.OutOrStdout(),
		errw:      cmd.ErrOrStderr(),
		cwd:       cwd,
		commonDir: common,
		mainRoot:  wts[0].Path,
		vcs:       newBackend(wts[0].Path),
		store:     &state.Store{Dir: filepath.Join(common, "bothways")},
	}
	a.cfg, err = config.Load(a.mainRoot)
	if err != nil {
		return nil, userErrorf("config: %v", err)
	}
	if cmd.Annotations[exclusiveAnnotation] == "true" {
		holder := fmt.Sprintf("pid %d: bothways %s", os.Getpid(), cmd.Name())
		release, err := a.store.Exclusive(holder, func(other string) {
			if other == "" {
				other = "another bothways command"
			}
			fmt.Fprintf(a.errw, "bothways: waiting for %s to finish\n", other)
		})
		if err != nil {
			return nil, envErrorf("%v", err)
		}
		if held, ok := ctx.Value(releasesKey{}).(*releases); ok {
			held.add(release)
		} else {
			release() // not run through Run: nothing would release it later
			return nil, envErrorf("bothways %s must run through cli.Run to take the operation lock", cmd.Name())
		}
	}
	return a, nil
}

// newBackend is the one place a backend is chosen. Reads of commit ids stay on
// gitx because every backend keeps a git repository next to its own.
func newBackend(mainRoot string) backend.Backend {
	return gitbackend.New(mainRoot)
}

func (a *app) warnf(format string, args ...any) {
	fmt.Fprintf(a.errw, "warning: "+format+"\n", args...)
}

func (a *app) infof(format string, args ...any) {
	fmt.Fprintf(a.errw, format+"\n", args...)
}

// load reads state.json without locking (writes are atomic renames).
func (a *app) load() (*state.State, error) {
	st, err := a.store.Load()
	if errors.Is(err, state.ErrCorrupt) {
		return nil, envErrorf("%v", err)
	}
	if err != nil {
		return nil, envErrorf("reading state: %v", err)
	}
	return st, nil
}

// update applies fn under the state lock.
func (a *app) update(fn func(*state.State) error) error {
	err := a.store.Update(fn)
	if errors.Is(err, state.ErrCorrupt) {
		return envErrorf("%v", err)
	}
	return err
}

// ensureInit creates the state directory and lets the backend prepare the
// repository (git turns rerere on unless the user configured it explicitly).
func (a *app) ensureInit() error {
	if err := os.MkdirAll(a.store.Dir, 0o755); err != nil {
		return envErrorf("creating %s: %v", a.store.Dir, err)
	}
	return a.vcs.Init(a.ctx)
}

// prepare prunes stale worktree entries and folds finished conflict
// resolutions back into the state. Write commands start with it.
func (a *app) prepare() (*state.State, error) {
	if err := a.vcs.PruneWorkspaces(a.ctx); err != nil {
		return nil, err
	}
	st, err := a.load()
	if err != nil {
		return nil, err
	}
	type resolution struct{ baseTip string }
	resolved := map[state.Ref]resolution{}
	for _, d := range st.Decisions {
		for _, v := range d.Variants {
			if r, ok := a.conflictResolved(v); ok {
				resolved[d.Ref(v)] = resolution{r}
			}
		}
	}
	if len(resolved) == 0 {
		return st, nil
	}
	err = a.update(func(st *state.State) error {
		for ref, r := range resolved {
			if v := st.Variant(ref); v != nil && v.State == state.Conflict {
				if r.baseTip != "" {
					v.BaseTip = r.baseTip
				}
				v.State, v.SyncOnto = state.Idle, ""
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return a.load()
}

// conflictResolved reports whether a variant marked conflict no longer holds
// one. baseTip is the new base tip when the conflict was resolved, "" when the
// operation was abandoned instead.
func (a *app) conflictResolved(v *state.Variant) (baseTip string, ok bool) {
	if v.State != state.Conflict || v.Worktree == "" || !dirExists(v.Worktree) {
		return "", false
	}
	if busy, err := a.vcs.InConflict(a.ctx, v.Worktree); err != nil || busy {
		return "", false
	}
	if v.SyncOnto != "" {
		if tip, ok, _ := gitx.BranchTip(a.ctx, a.mainRoot, v.Branch); ok {
			if done, _ := gitx.IsAncestor(a.ctx, a.mainRoot, v.SyncOnto, tip); done {
				return v.SyncOnto, true
			}
		}
	}
	return "", true
}

// parentBranch is the branch d's variants stack on: the base, or the parent variant.
func (a *app) parentBranch(st *state.State, d *state.Decision) (string, error) {
	if d.Under == nil {
		if st.BaseBranch == "" {
			return "", userErrorf("no base branch recorded; run `bothways doctor`")
		}
		return st.BaseBranch, nil
	}
	pv := st.Variant(*d.Under)
	if pv == nil {
		return "", userErrorf("decision %s is nested under %s, which is not open; run `bothways doctor`", d.ID, d.Under)
	}
	return pv.Branch, nil
}

// tipOf resolves a branch that must exist.
func (a *app) tipOf(branch string) (string, error) {
	tip, ok, err := gitx.BranchTip(a.ctx, a.mainRoot, branch)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", userErrorf("branch %s does not exist", branch)
	}
	return tip, nil
}

// worktreeOf returns the worktree where branch is checked out.
func (a *app) worktreeOf(branch string) (string, bool, error) {
	wts, err := a.vcs.Workspaces(a.ctx)
	if err != nil {
		return "", false, err
	}
	for _, wt := range wts {
		if wt.Branch == branch && !wt.Prunable {
			return wt.Path, true, nil
		}
	}
	return "", false, nil
}

// worktreeRoot is where variant worktrees live.
func (a *app) worktreeRoot(st *state.State) (string, error) {
	if root := a.cfg.ResolveRoot(a.mainRoot); root != "" {
		return root, nil
	}
	rev := "HEAD"
	if st.BaseBranch != "" {
		rev = "refs/heads/" + st.BaseBranch
	}
	id, err := worktree.RepoID(a.ctx, a.mainRoot, rev)
	if err != nil {
		return "", userErrorf("cannot compute repository id (does the repository have commits?): %v", err)
	}
	root, err := worktree.DefaultRoot(id)
	if err != nil {
		return "", envErrorf("%v", err)
	}
	return root, nil
}

// seedDir is the checkout gitignored paths are copied from.
func (a *app) seedDir(st *state.State) string {
	if st.BaseBranch != "" {
		if wt, ok, _ := a.worktreeOf(st.BaseBranch); ok {
			return wt
		}
	}
	return a.mainRoot
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

func now() time.Time { return time.Now().UTC().Truncate(time.Second) }

func short(oid string) string {
	if len(oid) > 7 {
		return oid[:7]
	}
	return oid
}

// description is attached to every variant branch so that `doctor` can
// rebuild decisions from branches alone.
func description(st *state.State, d *state.Decision) string {
	lines := []string{"bothways variant", "base=" + st.BaseBranch, "default=" + d.Default}
	if d.Under != nil {
		lines = append(lines, "under="+d.Under.String())
	}
	if q := strings.Join(strings.Fields(d.Question), " "); q != "" {
		lines = append(lines, "question="+q)
	}
	return strings.Join(lines, "\n")
}

func parseDescription(s string) map[string]string {
	fields := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			fields[strings.TrimSpace(k)] = v
		}
	}
	return fields
}

func (a *app) writeDescriptions(st *state.State, d *state.Decision) error {
	desc := description(st, d)
	for _, v := range d.Variants {
		if err := a.vcs.SetDescription(a.ctx, v.Branch, desc); err != nil {
			return err
		}
	}
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
