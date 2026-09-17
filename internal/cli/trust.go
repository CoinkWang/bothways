package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/CoinkWang/bothways/internal/config"
	"github.com/CoinkWang/bothways/internal/state"
	"github.com/CoinkWang/bothways/internal/trust"
)

// Trust states of the repository config file.
const (
	trustNotNeeded = "not-needed" // no file, or a file that runs nothing
	trustTrusted   = "trusted"
	trustUntrusted = "untrusted"
	trustChanged   = "changed" // trusted once, edited since
)

func newTrustCmd() *cobra.Command {
	var show, revoke, asJSON bool
	cmd := &cobra.Command{
		Use:   "trust [--show | --revoke]",
		Short: "Allow the commands in .bothways.toml to run",
		Long: `.bothways.toml can make bothways run shell commands: the worktree setup (or the
preset that supplies it), preview start, deploy and down scripts, the agent
runner, and the environment the previews get. None of them runs until you have
reviewed the file and trusted it:

    bothways trust --show     what the file runs, and whether it is trusted
    bothways trust            trust the file as it is now
    bothways trust --revoke   stop trusting it

Trust covers the exact content, so any edit needs a new trust. It is recorded
per user in $XDG_STATE_HOME/bothways/trust.json (~/.local/state by default),
never in the repository. Until the file is trusted, fork, add and sync create
worktrees without running setup, and up, restart, run and down scripts refuse
to start. A runner passed with run --runner is your own command and needs no
trust.`,
		Args: wrapArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openApp(cmd)
			if err != nil {
				return err
			}
			return a.runTrust(show, revoke, asJSON)
		},
	}
	cmd.Flags().BoolVar(&show, "show", false, "print what .bothways.toml runs and whether it is trusted, without changing anything")
	cmd.Flags().BoolVar(&revoke, "revoke", false, "stop trusting .bothways.toml")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	cmd.MarkFlagsMutuallyExclusive("show", "revoke")
	return cmd
}

func (a *app) runTrust(show, revoke, asJSON bool) error {
	repo := a.cfg.Repo
	store := trustStore()
	ts, err := a.trustState()
	if err != nil {
		return envErrorf("reading %s: %v", store.Path, err)
	}
	revoked := false
	switch {
	case show:
	case revoke:
		if repo.Path != "" {
			if revoked, err = store.Revoke(repo.Path); err != nil {
				return envErrorf("writing %s: %v", store.Path, err)
			}
			if ts != trustNotNeeded {
				ts = trustUntrusted
			}
		}
	case ts == trustUntrusted || ts == trustChanged:
		if err := store.Trust(repo.Path, repo.SHA256, now()); err != nil {
			return envErrorf("writing %s: %v", store.Path, err)
		}
		ts = trustTrusted
	}

	if asJSON {
		commands := repo.Commands
		if commands == nil {
			commands = []config.Command{}
		}
		return writeJSON(a.out, map[string]any{
			"schema_version": state.SchemaVersion,
			"file":           repo.Path,
			"sha256":         repo.SHA256,
			"state":          ts,
			"commands":       commands,
		})
	}
	switch {
	case revoke && revoked:
		fmt.Fprintf(a.out, "no longer trusting %s\n", repo.Path)
		return nil
	case revoke:
		fmt.Fprintf(a.out, "%s was not trusted\n", orDash(repo.Path))
		return nil
	case repo.Path == "":
		fmt.Fprintf(a.out, "no %s in %s: nothing to trust\n", config.FileName, a.mainRoot)
		return nil
	case len(repo.Commands) == 0:
		fmt.Fprintf(a.out, "%s runs no commands: nothing to trust\n", repo.Path)
		return nil
	}
	fmt.Fprintf(a.out, "%s runs:\n", repo.Path)
	tw := tabwriter.NewWriter(a.out, 0, 4, 2, ' ', 0)
	for _, c := range repo.Commands {
		from := ""
		if c.From != "" {
			from = "  (from " + c.From + ")"
		}
		fmt.Fprintf(tw, "  %s\t%s%s\n", c.Key, c.Value, from)
	}
	tw.Flush()
	fmt.Fprintf(a.out, "%s (sha256 %s)\n", trustLabel(ts), short(repo.SHA256))
	return nil
}

func trustLabel(s string) string {
	switch s {
	case trustTrusted:
		return "trusted"
	case trustChanged:
		return "changed since it was trusted: run `bothways trust` again"
	default:
		return "not trusted: run `bothways trust` to allow these commands"
	}
}

func trustStore() trust.Store { return trust.Store{Path: trust.DefaultPath()} }

// trustState compares the repository config file bothways loaded with what the
// user trusted.
func (a *app) trustState() (string, error) {
	repo := a.cfg.Repo
	if len(repo.Commands) == 0 {
		return trustNotNeeded, nil
	}
	sum, err := trustStore().Lookup(repo.Path)
	switch {
	case err != nil:
		return "", err
	case sum == "":
		return trustUntrusted, nil
	case sum == repo.SHA256:
		return trustTrusted, nil
	}
	return trustChanged, nil
}

// requireTrust fails a command that exists to run what the config file names.
// action completes "bothways will not ...".
func (a *app) requireTrust(action string) error {
	s, err := a.trustState()
	if err != nil {
		return envErrorf("reading %s: %v", trustStore().Path, err)
	}
	if s == trustTrusted || s == trustNotNeeded {
		return nil
	}
	return userErrorf("%s", a.untrusted(s, action))
}

// setupAllowed gates the setup command. Setup only ever degrades, so an
// untrusted config skips it with one warning per command instead of failing:
// the worktree is still created, and up runs setup once the file is trusted.
func (a *app) setupAllowed() bool {
	s, err := a.trustState()
	if err == nil && (s == trustTrusted || s == trustNotNeeded) {
		return true
	}
	if !a.setupSkipWarned {
		a.setupSkipWarned = true
		if err != nil {
			a.warnf("setup skipped: reading %s: %v", trustStore().Path, err)
		} else {
			a.warnf("setup skipped: %s", a.untrusted(s, "run setup"))
		}
	}
	return false
}

func (a *app) untrusted(s, action string) string {
	why := "is not trusted"
	if s == trustChanged {
		why = "changed since it was trusted"
	}
	return fmt.Sprintf("%s %s, so bothways will not %s; review it with `bothways trust --show`, then run `bothways trust`", a.cfg.Repo.Path, why, action)
}
