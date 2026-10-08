// Package cli implements the bothways command tree.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// Version is stamped by release builds with
// -ldflags "-X github.com/CoinkWang/bothways/internal/cli.Version=v1.2.3".
var Version = ""

// version falls back to the module version, which `go install ...@v1.2.3`
// records, and to "dev" for a plain checkout build.
func version() string {
	if Version != "" {
		return Version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

// Exit codes are part of the agent contract.
const (
	ExitOK      = 0
	ExitUser    = 1 // bad arguments, invalid state transition
	ExitPartial = 2 // conflict or partial failure
	ExitEnv     = 3 // git missing/too old, not a repository, IO trouble
)

// exitError carries an exit code through cobra.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

func userErrorf(format string, args ...any) error {
	return &exitError{ExitUser, fmt.Errorf(format, args...)}
}

func envErrorf(format string, args ...any) error {
	return &exitError{ExitEnv, fmt.Errorf(format, args...)}
}

func partialErrorf(format string, args ...any) error {
	return &exitError{ExitPartial, fmt.Errorf(format, args...)}
}

// ExitCodeOf maps an error to the process exit code.
func ExitCodeOf(err error) int {
	if err == nil {
		return ExitOK
	}
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	return ExitUser
}

// Main runs the CLI with os.Args and returns the exit code.
func Main() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return Run(ctx, os.Args[1:], os.Stdout, os.Stderr)
}

// Run executes one invocation; exposed for tests.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	root := NewRootCmd()
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	held := &releases{}
	defer held.run()
	err := root.ExecuteContext(context.WithValue(ctx, releasesKey{}, held))
	if err != nil {
		fmt.Fprintf(stderr, "bothways: %v\n", err)
	}
	return ExitCodeOf(err)
}

// NewRootCmd builds the command tree.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "bothways",
		Short: "Variant worktree orchestrator: build several approaches side by side, keep one by fast-forward",
		Long: `bothways keeps "several ways of doing the same task" as variant branches, each
checked out in its own worktree on top of a shared base branch. Pick one and the
base fast-forwards to it; the rest disappear without reverts.

Variants are always written <decision>=<variant>. Read commands accept --json.
Exit codes: 0 ok, 1 user error, 2 conflict or partial failure, 3 environment error.`,
		Version:       version(),
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetVersionTemplate("bothways {{.Version}}\n")
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return &exitError{ExitUser, err}
	})
	root.AddCommand(
		newInitCmd(),
		newForkCmd(),
		newAddCmd(),
		newTreeCmd(),
		newStatusCmd(),
		newPathCmd(),
		newSyncCmd(),
		newPickCmd(),
		newDropCmd(),
		newGCCmd(),
		newDoctorCmd(),
		newComboCmd(),
		newPresetCmd(),
		newDiffCmd(),
		newNoteCmd(),
		newUpCmd(),
		newDownCmd(),
		newRestartCmd(),
		newHoistCmd(),
		newRunCmd(),
		newCompareCmd(),
		newHookCmd(),
		newTrustCmd(),
		newSkillCmd(),
	)
	for _, cmd := range root.Commands() {
		if exclusiveCommands[cmd.Name()] {
			if cmd.Annotations == nil {
				cmd.Annotations = map[string]string{}
			}
			cmd.Annotations[exclusiveAnnotation] = "true"
		}
	}
	return root
}

// exclusiveCommands rewrite branches or worktrees, so only one of them runs
// per repository at a time: a manual sync and the one a post-commit hook starts
// in the background would otherwise rebase the same variant concurrently.
// Reads, note, run and compare do not take it.
var exclusiveCommands = map[string]bool{
	"fork": true, "add": true, "sync": true, "pick": true, "drop": true, "hoist": true,
	"combo": true, "gc": true, "doctor": true, "up": true, "down": true, "restart": true,
}

const exclusiveAnnotation = "bothways.exclusive"

// releases collects what one Run acquired, so the lock outlives RunE even when
// it returns an error, and in-process Runs (the hook's sync) keep their own.
type releases struct{ fns []func() }

type releasesKey struct{}

func (r *releases) add(fn func()) { r.fns = append(r.fns, fn) }

func (r *releases) run() {
	for i := len(r.fns) - 1; i >= 0; i-- {
		r.fns[i]()
	}
}

// wrapArgs turns cobra's positional-argument errors into user errors.
func wrapArgs(v cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := v(cmd, args); err != nil {
			return &exitError{ExitUser, err}
		}
		return nil
	}
}
