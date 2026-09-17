package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"text/template"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"github.com/CoinkWang/bothways/internal/config"
	"github.com/CoinkWang/bothways/internal/state"
	"github.com/CoinkWang/bothways/internal/worktree"
)

// runnerGrace is how long a runner has to wind down after Ctrl-C (SIGINT)
// before it is killed, matching the preview shutdown grace.
const runnerGrace = 10 * time.Second

// runVars are the variables a [run] runner template is rendered with.
type runVars struct {
	Worktree   string
	Decision   string
	Variant    string
	PromptFile string
}

type runTarget struct {
	ref     state.Ref
	vars    runVars
	branch  string
	command string
	log     string
}

type runResult struct {
	name    string
	outcome string
	detail  string
}

// Runner outcomes.
const (
	outOK          = "ok"
	outRunFailed   = "failed"
	outInterrupted = "interrupted"
)

func newRunCmd() *cobra.Command {
	var runner string
	var jobs int
	var dryRun, asJSON bool
	cmd := &cobra.Command{
		Use:   `run [--runner "<template>"] [--jobs N] [<decision>=<variant> ...]`,
		Short: "Run an agent runner in each variant worktree",
		Long: `run starts one agent runner per target variant so the variants are filled in
parallel. The command template comes from [run] runner in .bothways.toml (or
--runner) and is rendered per variant with .Worktree, .Decision, .Variant and
.PromptFile; it runs through the shell with the worktree as its working
directory. Each runner's output goes to a log file next to its prompt file.

Without arguments the targets are every open, materialized variant that has no
commits of its own yet. The set of variants is finite and defined by whoever
forked the decision: run makes one pass over it, it is not a scheduler.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openApp(cmd)
			if err != nil {
				return err
			}
			if runner == "" {
				runner = a.cfg.Run.Runner
			}
			if strings.TrimSpace(runner) == "" {
				return userErrorf("no runner configured: set [run] runner in %s (or ~/.config/bothways/config.toml), or pass --runner \"<template>\"", config.FileName)
			}
			tmpl, err := template.New("runner").Parse(runner)
			if err != nil {
				return userErrorf("runner template: %v", err)
			}
			if !cmd.Flags().Changed("jobs") {
				jobs = a.cfg.Run.Jobs
			}
			if jobs < 1 {
				return userErrorf("--jobs %d must be at least 1", jobs)
			}
			if asJSON && !dryRun {
				return userErrorf("--json describes what run would do; pass it with --dry-run")
			}

			view, st, err := a.buildView()
			if err != nil {
				return err
			}
			targets, err := a.runTargets(view, st, args, tmpl)
			if err != nil {
				return err
			}
			if dryRun {
				return a.printRunPlan(targets, runner, jobs, asJSON)
			}
			if !cmd.Flags().Changed("runner") {
				// --runner is the user's own command; [run] runner is the config's.
				if err := a.requireTrust("start runners"); err != nil {
					return err
				}
			}
			if len(targets) == 0 {
				a.infof("no variant needs a runner (name one as <decision>=<variant> to run it anyway)")
				return nil
			}
			if err := os.MkdirAll(a.runDir(), 0o755); err != nil {
				return envErrorf("creating %s: %v", a.runDir(), err)
			}
			baseDir := a.seedDir(st)
			for _, t := range targets {
				if err := os.WriteFile(t.vars.PromptFile, []byte(promptFor(st, t, baseDir)), 0o644); err != nil {
					return envErrorf("writing prompt file: %v", err)
				}
			}
			return a.runAll(targets, jobs)
		},
	}
	cmd.Flags().StringVar(&runner, "runner", "", "command template to run per variant (overrides [run] runner)")
	cmd.Flags().IntVar(&jobs, "jobs", 0, "how many runners may run at once (default from [run] jobs)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the rendered commands without running anything")
	cmd.Flags().BoolVar(&asJSON, "json", false, "with --dry-run, print JSON")
	return cmd
}

// runTargets resolves the variants to run for: the arguments when given,
// otherwise every open variant that has a worktree and no commits of its own.
func (a *app) runTargets(view *statusView, st *state.State, args []string, tmpl *template.Template) ([]*runTarget, error) {
	views := map[state.Ref]*variantView{}
	for _, d := range view.Decisions {
		for _, v := range d.Variants {
			views[state.Ref{Decision: d.ID, Variant: v.ID}] = v
		}
	}
	var refs []state.Ref
	if len(args) > 0 {
		seen := map[state.Ref]bool{}
		for _, arg := range args {
			ref, err := st.Resolve(arg)
			if err != nil {
				return nil, userErrorf("%v", err)
			}
			vv := views[ref]
			switch {
			case vv == nil:
				return nil, userErrorf("%s is not in the current status; run `bothways doctor`", ref)
			case vv.Worktree == "":
				return nil, userErrorf("%s has no worktree to run in (it is %s)", ref, vv.State)
			case !dirExists(vv.Worktree):
				return nil, userErrorf("worktree of %s is missing (%s); run `bothways doctor`", ref, vv.Worktree)
			case vv.State == state.Conflict:
				return nil, userErrorf("%s is in conflict; resolve the %s in %s first", ref, a.vcs.Vocab().Stopped, vv.Worktree)
			}
			if !seen[ref] {
				seen[ref], refs = true, append(refs, ref)
			}
		}
	} else {
		for _, d := range view.Decisions {
			for _, v := range d.Variants {
				if v.State == state.Idle && v.Ahead == 0 {
					refs = append(refs, state.Ref{Decision: d.ID, Variant: v.ID})
				}
			}
		}
	}

	dir := a.runDir()
	targets := make([]*runTarget, 0, len(refs))
	for _, ref := range refs {
		vv := views[ref]
		t := &runTarget{
			ref:    ref,
			branch: vv.Branch,
			log:    filepath.Join(dir, ref.Name()+".log"),
			vars: runVars{
				Worktree:   vv.Worktree,
				Decision:   ref.Decision,
				Variant:    ref.Variant,
				PromptFile: filepath.Join(dir, ref.Name()+".prompt.md"),
			},
		}
		var b strings.Builder
		if err := tmpl.Execute(&b, t.vars); err != nil {
			return nil, userErrorf("rendering the runner for %s: %v", ref, err)
		}
		t.command = b.String()
		targets = append(targets, t)
	}
	return targets, nil
}

// runDir holds the per-variant prompt and log files.
func (a *app) runDir() string { return filepath.Join(a.store.Dir, "run") }

func (a *app) printRunPlan(targets []*runTarget, runner string, jobs int, asJSON bool) error {
	if !asJSON {
		for _, t := range targets {
			fmt.Fprintf(a.out, "%s\t%s\n", t.ref, t.command)
		}
		return nil
	}
	type plan struct {
		Name       string `json:"name"`
		Decision   string `json:"decision"`
		Variant    string `json:"variant"`
		Worktree   string `json:"worktree"`
		PromptFile string `json:"prompt_file"`
		Log        string `json:"log"`
		Command    string `json:"command"`
	}
	plans := []plan{}
	for _, t := range targets {
		plans = append(plans, plan{
			Name: t.ref.String(), Decision: t.ref.Decision, Variant: t.ref.Variant,
			Worktree: t.vars.Worktree, PromptFile: t.vars.PromptFile, Log: t.log, Command: t.command,
		})
	}
	return writeJSON(a.out, map[string]any{
		"schema_version": state.SchemaVersion,
		"runner":         runner,
		"jobs":           jobs,
		"runs":           plans,
	})
}

// runAll runs every target, at most jobs at a time, and reports each one's
// exit status. One pass, no queue: the variant set does not grow while we run.
func (a *app) runAll(targets []*runTarget, jobs int) error {
	a.infof("running %d variant(s), %d at a time", len(targets), jobs)
	results := make([]runResult, len(targets))
	var g errgroup.Group
	g.SetLimit(jobs)
	for i, t := range targets {
		g.Go(func() error {
			results[i] = a.runOne(t)
			return nil
		})
	}
	g.Wait() // runOne never returns an error: every runner gets its turn
	failed := 0
	for _, r := range results {
		if r.outcome != outOK {
			failed++
		}
		fmt.Fprintf(a.out, "%s\t%s\t%s\n", r.name, r.outcome, r.detail)
	}
	if failed > 0 {
		return partialErrorf("%d of %d runner(s) failed", failed, len(results))
	}
	return nil
}

func (a *app) runOne(t *runTarget) runResult {
	res := runResult{name: t.ref.String(), outcome: outOK, detail: t.log}
	logFile, err := os.Create(t.log)
	if err != nil {
		return runResult{name: t.ref.String(), outcome: outRunFailed, detail: fmt.Sprintf("cannot write log: %v", err)}
	}
	defer logFile.Close()
	fmt.Fprintf(logFile, "$ %s\n", t.command)

	shell, flag := "sh", "-c"
	if runtime.GOOS == "windows" {
		shell, flag = "cmd", "/C"
	}
	cmd := exec.CommandContext(a.ctx, shell, flag, t.command)
	cmd.Dir = t.vars.Worktree
	spec := worktree.Spec{Path: t.vars.Worktree, Decision: t.ref.Decision, Variant: t.ref.Variant}
	cmd.Env = append(os.Environ(), spec.Env()...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	// Ctrl-C cancels a.ctx: ask the runner to stop, kill it if it will not.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = runnerGrace

	a.infof("%s: %s", t.ref, t.command)
	err = cmd.Run()
	if err == nil {
		return res
	}
	res.outcome, res.detail = outRunFailed, fmt.Sprintf("%v; see %s", err, t.log)
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		res.detail = fmt.Sprintf("exit %d; see %s", ee.ExitCode(), t.log)
	}
	if a.ctx.Err() != nil {
		res.outcome, res.detail = outInterrupted, "see "+t.log
	}
	return res
}

// promptFor is what the runner is told: the question, which variant it
// is meant to build, and the working discipline — changes that belong
// to every variant are committed in the base worktree, this variant's own
// changes here, and a one-line note when it is done.
func promptFor(st *state.State, t *runTarget, baseDir string) string {
	ref := t.ref
	d := st.Decision(ref.Decision)
	var b strings.Builder
	fmt.Fprintf(&b, "# bothways variant %s\n\n", ref)
	if d != nil && d.Question != "" {
		fmt.Fprintf(&b, "The open question is: %s\n\n", d.Question)
	}
	fmt.Fprintf(&b, "Build the %q answer to it, and only that one. Do not hedge towards the\nothers and do not ask which one is wanted: the point of this decision is that\nevery answer gets built, and a human compares them afterwards.\n", ref.Variant)
	if d != nil {
		var siblings []string
		for _, v := range d.Variants {
			if v.ID != ref.Variant {
				siblings = append(siblings, v.ID)
			}
		}
		if len(siblings) > 0 {
			fmt.Fprintf(&b, "\nThe other answers (%s) are being built in parallel by other runners.\nLeave their worktrees alone.\n", strings.Join(siblings, ", "))
		}
	}
	b.WriteString("\n## Where to commit what\n\n")
	fmt.Fprintf(&b, "- This variant lives in %s, on branch %s. You are already in it.\n", t.vars.Worktree, t.branch)
	fmt.Fprintf(&b, "- The shared base lives in %s, on branch %s.\n", baseDir, orDash(st.BaseBranch))
	fmt.Fprintf(&b, "- A change that every variant would need (a shared fix, a refactor, a\n  dependency) belongs in the base worktree: commit it there, not here.\n")
	fmt.Fprintf(&b, "- A change that is specific to %q belongs here: plain git commits in this\n  worktree, no merges and no rebases.\n", ref.Variant)
	b.WriteString("\n## When you are done\n\n")
	fmt.Fprintf(&b, "Leave one line saying what you built, so the human comparing the variants\nknows what they are looking at:\n\n    bothways note %s \"...\"\n", ref)
	return b.String()
}
