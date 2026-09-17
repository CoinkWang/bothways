package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/CoinkWang/bothways/internal/gitx"
	"github.com/CoinkWang/bothways/internal/state"
)

const (
	// hookMarker identifies a post-commit hook bothways wrote, so install
	// can be repeated and uninstall never deletes someone else's hook.
	hookMarker = "# bothways-managed-hook"
	// chainedSuffix names the pre-existing hook ours runs after itself.
	chainedSuffix = ".pre-bothways"
	hookLogName   = "hook.log"
	hookLockName  = "hook.lock"
	hookLogMax    = 1 << 20
	// maxHookSyncs bounds how often one trigger re-syncs while the base keeps
	// moving underneath it.
	maxHookSyncs = 5
)

// hookEnvVars are what git exports into a hook. The background sync outlives
// the commit and may run setup commands, which must see the worktree they run
// in, not the one the commit happened in.
var hookEnvVars = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_PREFIX", "GIT_COMMON_DIR",
	"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE",
	"GIT_REFLOG_ACTION",
}

// hookScript is the post-commit hook. It must never slow or fail the commit:
// the sync runs in the background with every stream detached, since a GUI
// client waits for the hook's output pipes to close, not just for the hook.
//
// The branch test is a prefilter that avoids starting a process at all for
// commits that cannot be on the base: those in variant worktrees and those a
// rebase replays with HEAD detached (the sequencer runs post-commit for each).
// `bothways hook post-commit` checks the recorded base itself.
func hookScript(bin string) string {
	return `#!/bin/sh
` + hookMarker + `
# Installed by ` + "`bothways init --hooks`" + `: a commit on the base branch restacks
# the variants in the background (log: $GIT_DIR/bothways/` + hookLogName + `).
# ` + "`bothways init --no-hooks`" + ` removes this file and restores the hook it chains.
bothways=` + shellQuote(bin) + `

case "$(git symbolic-ref -q HEAD 2>/dev/null)" in
"" | refs/heads/` + state.BranchPrefix + `*) ;;
*)
	if [ -x "$bothways" ]; then
		"$bothways" hook post-commit </dev/null >/dev/null 2>&1 &
	else
		log="$(git rev-parse --git-common-dir 2>/dev/null)/bothways/` + hookLogName + `"
		(echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) post-commit: $bothways is missing; sync skipped (run ` + "`bothways init --hooks`" + ` again)" >>"$log") 2>/dev/null
	fi
	;;
esac

chained="${0%/*}/post-commit` + chainedSuffix + `"
if [ -x "$chained" ]; then
	exec "$chained" "$@"
fi
exit 0
`
}

// hookLine is what a hook manager's post-commit should run instead.
func hookLine(bin string) string {
	return shellQuote(bin) + " hook post-commit </dev/null >/dev/null 2>&1 &"
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// hookBinary extracts the recorded bothways path from one of our scripts.
func hookBinary(script []byte) string {
	for _, line := range strings.Split(string(script), "\n") {
		if v, ok := strings.CutPrefix(line, "bothways='"); ok {
			return strings.ReplaceAll(strings.TrimSuffix(v, "'"), `'\''`, "'")
		}
	}
	return ""
}

func isOurHook(path string) bool {
	data, err := os.ReadFile(path)
	return err == nil && bytes.Contains(data, []byte(hookMarker))
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// selfPath is the bothways the hook will run. The path it was invoked by is
// preferred over the resolved executable: a package manager's symlink
// (Homebrew's bin/bothways) survives upgrades, the versioned file behind it
// does not.
func selfPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	arg, cand := os.Args[0], ""
	if strings.ContainsAny(arg, `/\`) {
		cand, _ = filepath.Abs(arg)
	} else if p, err := exec.LookPath(arg); err == nil {
		cand, _ = filepath.Abs(p)
	}
	if cand != "" {
		if a, err := os.Stat(cand); err == nil {
			if b, err := os.Stat(exe); err == nil && os.SameFile(a, b) {
				exe = cand
			}
		}
	}
	if exe, err = filepath.Abs(exe); err != nil {
		return "", err
	}
	if runtime.GOOS == "windows" {
		exe = filepath.ToSlash(exe) // the hook runs under Git for Windows' sh
	}
	return exe, nil
}

// hooksDir resolves the hooks directory the way git does, honouring
// core.hooksPath.
func (a *app) hooksDir() (string, error) {
	dir, err := gitx.GitPath(a.ctx, a.mainRoot, "hooks")
	if err != nil {
		return "", err
	}
	return filepath.Clean(dir), nil
}

func within(path, root string) bool {
	rel, err := filepath.Rel(canonicalDir(root), canonicalDir(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func canonicalDir(p string) string {
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	return filepath.Clean(p)
}

// checkHooksDir refuses a hooks directory outside the git dir. Inside a
// working tree it is a tracked file of the user's (husky, lefthook); anywhere
// else it is shared by other repositories. Either way the user's own hook
// setup should call bothways, so the line to add goes to stdout.
func (a *app) checkHooksDir(dir, bin string) error {
	if within(dir, a.commonDir) {
		return nil
	}
	where := "outside this repository, where other repositories share it"
	if wts, err := gitx.Worktrees(a.ctx, a.mainRoot); err == nil {
		for _, wt := range wts {
			if within(dir, wt.Path) {
				where = "inside the working tree, so installing would change a file in your repository"
				break
			}
		}
	}
	fmt.Fprintln(a.out, hookLine(bin))
	return userErrorf("core.hooksPath points at %s, %s; not installing. Add the line above to your post-commit hook instead", dir, where)
}

// installHook writes our post-commit hook, moving a foreign one aside so ours
// chains to it.
func (a *app) installHook(dir, bin string) error {
	path := filepath.Join(dir, "post-commit")
	saved := path + chainedSuffix
	want := hookScript(bin)
	logPath := filepath.Join(a.store.Dir, hookLogName)
	switch {
	case isOurHook(path):
		if cur, err := os.ReadFile(path); err == nil && string(cur) == want {
			fmt.Fprintf(a.out, "post-commit hook already installed: %s\n", path)
			return nil
		}
		if err := writeHook(path, want); err != nil {
			return envErrorf("writing %s: %v", path, err)
		}
		fmt.Fprintf(a.out, "updated post-commit hook %s (runs %s; log: %s)\n", path, bin, logPath)
		return nil
	case exists(path):
		if exists(saved) {
			return userErrorf("%s is not a bothways hook and %s already holds the hook bothways chains to; move one of them away first", path, saved)
		}
		if err := os.Rename(path, saved); err != nil {
			return envErrorf("moving %s aside: %v", path, err)
		}
		if err := writeHook(path, want); err != nil {
			os.Rename(saved, path)
			return envErrorf("writing %s: %v", path, err)
		}
		fmt.Fprintf(a.out, "installed post-commit hook %s (runs %s; log: %s)\n", path, bin, logPath)
		fmt.Fprintf(a.out, "chained existing post-commit hook, kept as %s\n", saved)
		return nil
	default:
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return envErrorf("creating %s: %v", dir, err)
		}
		if err := writeHook(path, want); err != nil {
			return envErrorf("writing %s: %v", path, err)
		}
		fmt.Fprintf(a.out, "installed post-commit hook %s (runs %s; log: %s)\n", path, bin, logPath)
		return nil
	}
}

func writeHook(path, content string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".post-commit.tmp*")
	if err != nil {
		return err
	}
	_, err = tmp.WriteString(content)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o755)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}

// uninstallHooks removes our hook and puts back whatever it chained. Besides
// the resolved hooks directory it looks in the git dir's own, where the hook
// stays after core.hooksPath is pointed elsewhere; it never touches a
// directory outside the git dir.
func (a *app) uninstallHooks() error {
	dirs := []string{filepath.Join(a.commonDir, "hooks")}
	if dir, err := a.hooksDir(); err == nil && within(dir, a.commonDir) && canonicalDir(dir) != canonicalDir(dirs[0]) {
		dirs = append([]string{dir}, dirs...)
	}
	found := false
	for _, dir := range dirs {
		path := filepath.Join(dir, "post-commit")
		saved := path + chainedSuffix
		switch {
		case isOurHook(path):
			if err := os.Remove(path); err != nil {
				return envErrorf("removing %s: %v", path, err)
			}
			fmt.Fprintf(a.out, "removed post-commit hook %s\n", path)
		case exists(saved) && !exists(path):
			// Our hook was deleted by hand; the chained one is still owed back.
		case exists(saved):
			return userErrorf("%s is not a bothways hook, so %s cannot be restored over it; merge them by hand", path, saved)
		default:
			continue
		}
		found = true
		if exists(saved) {
			if err := os.Rename(saved, path); err != nil {
				return envErrorf("restoring %s: %v", path, err)
			}
			fmt.Fprintf(a.out, "restored previous post-commit hook %s\n", path)
		}
	}
	if !found {
		fmt.Fprintf(a.out, "no bothways post-commit hook installed\n")
	}
	return nil
}

// hooksView tells status readers whether commits on the base sync on their own.
type hooksView struct {
	PostCommit bool   `json:"post_commit"`
	Path       string `json:"path,omitempty"`
	Binary     string `json:"binary,omitempty"`
	// BinaryOK is false when the recorded bothways is gone: the hook then only
	// logs that it skipped. `bothways init --hooks` records the current one.
	BinaryOK bool   `json:"binary_ok"`
	Chained  string `json:"chained,omitempty"`
	Log      string `json:"log,omitempty"`
}

func (a *app) hooksView() *hooksView {
	hv := &hooksView{}
	dir, err := a.hooksDir()
	if err != nil {
		return hv
	}
	path := filepath.Join(dir, "post-commit")
	script, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(script, []byte(hookMarker)) {
		return hv
	}
	hv.PostCommit, hv.Path = true, path
	hv.Binary = hookBinary(script)
	if info, err := os.Stat(filepath.FromSlash(hv.Binary)); err == nil && !info.IsDir() {
		hv.BinaryOK = true
	}
	if exists(path + chainedSuffix) {
		hv.Chained = path + chainedSuffix
	}
	hv.Log = filepath.Join(a.store.Dir, hookLogName)
	return hv
}

func newHookCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "hook post-commit",
		Short:  "Run what an installed git hook triggers (called by the hook)",
		Hidden: true,
		Args:   wrapArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if args[0] != "post-commit" {
				return userErrorf("unknown hook %q", args[0])
			}
			runPostCommit(cmd.Context())
			return nil
		},
	}
}

// runPostCommit syncs after a commit on the base branch. Nothing it does may
// reach the committer, so every failure ends up in the log or nowhere.
//
// Triggers coalesce: only one hook sync runs at a time, a trigger that finds
// one running leaves, and the running one keeps syncing until the base tip
// stops moving. It checks the tip once more after releasing the lock, which
// closes the gap in which a trigger could leave without being seen.
func runPostCommit(ctx context.Context) {
	detachSession()
	for _, name := range hookEnvVars {
		os.Unsetenv(name)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return
	}
	branch, err := gitx.CurrentBranch(ctx, cwd)
	if err != nil || branch == "" || strings.HasPrefix(branch, state.BranchPrefix) {
		return
	}
	common, err := gitx.CommonDir(ctx, cwd)
	if err != nil {
		return
	}
	store := &state.Store{Dir: filepath.Join(common, "bothways")}
	if !store.Exists() {
		return
	}
	if st, err := store.Load(); err != nil || st.BaseBranch != branch {
		return
	}
	logw, err := openHookLog(store.Dir)
	if err != nil {
		return
	}
	defer logw.Close()
	logf := func(format string, args ...any) {
		fmt.Fprintf(logw, "%s %s\n", time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(format, args...))
	}
	tip := func() string {
		t, _, _ := gitx.BranchTip(ctx, cwd, branch)
		return t
	}

	synced, runs := "", 0
	for runs < maxHookSyncs {
		unlock, ok, err := tryLock(filepath.Join(store.Dir, hookLockName))
		if err != nil {
			logf("post-commit on %s: %v", branch, err)
			return
		}
		if !ok {
			logf("post-commit on %s %s: a sync is already running and will restack onto it", branch, tip())
			return
		}
		for ; runs < maxHookSyncs; runs++ {
			onto := tip()
			if onto == synced {
				break
			}
			logf("post-commit on %s %s: sync", branch, onto)
			// --skip-dirty: nobody asked for this sync in the moment, so it must
			// not stash and rewrite a worktree someone is editing.
			code := Run(ctx, []string{"sync", "--skip-dirty"}, logw, logw)
			logf("sync onto %s exited %d", onto, code)
			synced = onto
		}
		unlock()
		if tip() == synced {
			return
		}
	}
	logf("post-commit on %s: base kept moving after %d syncs; stopped, run `bothways sync`", branch, runs)
}

func openHookLog(dir string) (*os.File, error) {
	path := filepath.Join(dir, hookLogName)
	if info, err := os.Stat(path); err == nil && info.Size() > hookLogMax {
		os.Rename(path, path+".1")
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}
