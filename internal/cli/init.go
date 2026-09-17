package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/CoinkWang/bothways/internal/gitx"
	"github.com/CoinkWang/bothways/internal/state"
)

func newInitCmd() *cobra.Command {
	var hooks, noHooks bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create $GIT_DIR/bothways and enable rerere",
		Long: `Create $GIT_DIR/bothways and enable rerere.

--hooks also installs a post-commit hook: a commit on the base branch restacks
the variants in the background, logging to $GIT_DIR/bothways/hook.log. A
post-commit hook that was already there keeps running after it. --no-hooks
removes the hook and restores the one it replaced.`,
		Args: wrapArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := openApp(cmd)
			if err != nil {
				return err
			}
			if noHooks {
				return a.uninstallHooks()
			}
			// Refuse before initializing, so a refused install changes nothing.
			var hooksDir, bin string
			if hooks {
				if bin, err = selfPath(); err != nil {
					return envErrorf("locating the bothways executable: %v", err)
				}
				if hooksDir, err = a.hooksDir(); err != nil {
					return err
				}
				if err := a.checkHooksDir(hooksDir, bin); err != nil {
					return err
				}
			}
			if err := a.ensureInit(); err != nil {
				return err
			}
			if !a.store.Exists() {
				branch, err := gitx.CurrentBranch(a.ctx, a.cwd)
				if err != nil {
					return err
				}
				err = a.update(func(st *state.State) error {
					if !strings.HasPrefix(branch, state.BranchPrefix) {
						st.BaseBranch = branch
					}
					return nil
				})
				if err != nil {
					return err
				}
			}
			st, err := a.load()
			if err != nil {
				return err
			}
			fmt.Fprintf(a.out, "initialized %s (base: %s)\n", a.store.Path(), orDash(st.BaseBranch))
			if !hooks {
				return nil
			}
			if st.BaseBranch == "" {
				return userErrorf("no base branch recorded, so the hook would never sync; run `bothways init` on the base branch first")
			}
			return a.installHook(hooksDir, bin)
		},
	}
	cmd.Flags().BoolVar(&hooks, "hooks", false, "install a post-commit hook that syncs after each commit on the base branch")
	cmd.Flags().BoolVar(&noHooks, "no-hooks", false, "remove that hook and restore the post-commit hook it chained")
	cmd.MarkFlagsMutuallyExclusive("hooks", "no-hooks")
	return cmd
}
