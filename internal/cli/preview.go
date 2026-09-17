package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/CoinkWang/bothways/internal/config"
	"github.com/CoinkWang/bothways/internal/preview"
	"github.com/CoinkWang/bothways/internal/state"
)

const (
	// stopGrace is how long a preview has between SIGTERM and SIGKILL.
	stopGrace = 10 * time.Second
	// healthInterval is the health polling period.
	healthInterval = 500 * time.Millisecond
)

// Preview outcomes; outFailed and outSkipped are shared with sync.
const (
	outUp        = "up"
	outDown      = "down"
	outUnchanged = "unchanged"
)

type previewResult struct {
	Ref     state.Ref `json:"-"`
	Name    string    `json:"name"`
	Outcome string    `json:"outcome"`
	Slot    int       `json:"slot,omitempty"`
	URL     string    `json:"url,omitempty"`
	Detail  string    `json:"detail,omitempty"`
}

func newUpCmd() *cobra.Command {
	var all, redeploy, asJSON bool
	cmd := &cobra.Command{
		Use:   "up [<decision>=<variant> ...]",
		Short: "Sync each variant, set its worktree up and start its preview",
		Long: `up brings previews of the named variants (every open one by default) up.

Each variant is synced onto the latest base first, so what you look at is never
stale. Setup runs unless the [worktree] fingerprint files are unchanged. In
start mode bothways supervises the process and prints its URL; in deploy mode it
runs the deploy script once and reads the URL from its last output line.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openApp(cmd)
			if err != nil {
				return err
			}
			return a.runUp(args, all, redeploy, asJSON)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "every open variant (the default when none is named)")
	cmd.Flags().BoolVar(&redeploy, "redeploy", false, "in deploy mode, deploy again even when the environment is already up")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func newDownCmd() *cobra.Command {
	var all, asJSON bool
	cmd := &cobra.Command{
		Use:   "down [<decision>=<variant> ...]",
		Short: "Stop previews and release their slots",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openApp(cmd)
			if err != nil {
				return err
			}
			results, err := a.runDown(args, all)
			if err != nil {
				return err
			}
			return a.reportPreview(results, asJSON, nil)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "every open variant (the default when none is named)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func newRestartCmd() *cobra.Command {
	var all, asJSON bool
	cmd := &cobra.Command{
		Use:   "restart [<decision>=<variant> ...]",
		Short: "Stop previews and start them again",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openApp(cmd)
			if err != nil {
				return err
			}
			if a.cfg.Preview.Mode() != config.ModeNone {
				if err := a.requireTrust("restart previews"); err != nil {
					return err
				}
			}
			if _, err := a.runDown(args, all); err != nil {
				return err
			}
			// A restart is an explicit request for a fresh deployment too.
			return a.runUp(args, all, true, asJSON)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "every open variant (the default when none is named)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func (a *app) runUp(args []string, all, redeploy, asJSON bool) error {
	if a.cfg.Preview.Mode() == config.ModeNone {
		return userErrorf("no preview configured: set start or deploy under [preview] in %s", config.FileName)
	}
	if err := a.requireTrust("start previews"); err != nil {
		return err
	}
	st, err := a.prepare()
	if err != nil {
		return err
	}
	targets, err := a.previewTargets(st, args, all)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return userErrorf("no open variant has a worktree to start")
	}

	// A preview is always based on the latest base.
	syncResults, err := a.sync(targets)
	if err != nil {
		return err
	}
	reportSync(a.errw, syncResults, true)
	blocked := map[state.Ref]string{}
	for _, r := range syncResults {
		if !r.ok() {
			blocked[r.Ref] = r.Outcome + ": " + r.Detail
		}
	}
	if _, err := a.reconcile(nil); err != nil {
		return err
	}
	if _, err := a.refreshPreviews(); err != nil {
		return err
	}

	// Variants of this same command never make room for each other: with more
	// targets than slots the user has to say which ones matter.
	mine := map[state.Ref]bool{}
	for _, ref := range targets {
		mine[ref] = true
	}
	var results []previewResult
	var fatal error
	for _, ref := range targets {
		if detail, ok := blocked[ref]; ok {
			results = append(results, previewResult{Ref: ref, Name: ref.String(), Outcome: outSkipped, Detail: detail})
			continue
		}
		r, err := a.upOne(ref, redeploy, mine)
		results = append(results, r)
		if err != nil {
			fatal = err
			break
		}
	}
	return a.reportPreview(results, asJSON, fatal)
}

func (a *app) runDown(args []string, all bool) ([]previewResult, error) {
	st, err := a.prepare()
	if err != nil {
		return nil, err
	}
	targets, err := a.previewTargets(st, args, all)
	if err != nil {
		return nil, err
	}
	if st, err = a.refreshPreviews(); err != nil {
		return nil, err
	}
	if err := a.checkDownScripts(st, targets); err != nil {
		return nil, err
	}
	var results []previewResult
	for _, ref := range targets {
		r := previewResult{Ref: ref, Name: ref.String(), Outcome: outDown}
		if _, err := a.stopPreview(ref); err != nil {
			r.Outcome, r.Detail = outFailed, err.Error()
		}
		results = append(results, r)
	}
	return results, nil
}

// previewTargets resolves the variants a preview command acts on. Without
// arguments (or with --all) that is every variant that has a worktree.
func (a *app) previewTargets(st *state.State, args []string, all bool) ([]state.Ref, error) {
	if len(args) > 0 {
		if all {
			return nil, userErrorf("--all takes no variant arguments")
		}
		refs := make([]state.Ref, 0, len(args))
		for _, arg := range args {
			ref, err := st.Resolve(arg)
			if err != nil {
				return nil, userErrorf("%v", err)
			}
			refs = append(refs, ref)
		}
		return refs, nil
	}
	var refs []state.Ref
	for _, d := range st.Ordered() {
		if !st.Materialized(d) {
			continue
		}
		for _, v := range d.Variants {
			refs = append(refs, d.Ref(v))
		}
	}
	return refs, nil
}

// upOne brings one variant's preview up. A returned error is fatal for the
// whole command (an exhausted budget, a broken template); trouble that only
// concerns this variant is reported in the result.
func (a *app) upOne(ref state.Ref, redeploy bool, mine map[state.Ref]bool) (previewResult, error) {
	res := previewResult{Ref: ref, Name: ref.String()}
	fail := func(format string, args ...any) previewResult {
		res.Outcome, res.Detail = outFailed, fmt.Sprintf(format, args...)
		return res
	}
	st, err := a.load()
	if err != nil {
		return res, err
	}
	v := st.Variant(ref)
	switch {
	case v == nil:
		return fail("no longer open"), nil
	case v.State == state.Deferred || v.Worktree == "":
		return fail("deferred: its parent variant is not the default, so it has no worktree"), nil
	case !dirExists(v.Worktree):
		return fail("worktree %s is missing; run `bothways doctor`", v.Worktree), nil
	}
	mode := a.cfg.Preview.Mode()
	if p := v.Preview; p != nil && v.HoldsSlot() && p.State == state.PreviewUp && !(mode == config.ModeDeploy && redeploy) {
		res.Outcome, res.URL, res.Slot = outUnchanged, p.URL, v.Slot
		return res, nil
	}
	slot := v.Slot
	if !v.HoldsSlot() {
		if slot, err = a.acquireSlot(ref, mode, mine); err != nil {
			return res, err
		}
	}
	if err := a.ensureSetup(ref, v.Worktree); err != nil {
		return res, err
	}
	vars := preview.Vars{
		Slot: slot, Port: a.cfg.Preview.BasePort + slot, Name: ref.Name(),
		Decision: ref.Decision, Variant: ref.Variant, Worktree: v.Worktree,
	}
	env, err := preview.RenderEnv(a.cfg.Preview.Env, vars)
	if err != nil {
		return res, userErrorf("%v", err)
	}
	logPath, pidPath, err := a.previewFiles(ref)
	if err != nil {
		return res, err
	}
	spec := preview.Spec{Dir: v.Worktree, Env: env, Log: logPath}
	if mode == config.ModeDeploy {
		return a.deployOne(ref, res, spec, vars, slot)
	}
	return a.startOne(ref, res, spec, vars, slot, pidPath)
}

func (a *app) startOne(ref state.Ref, res previewResult, spec preview.Spec, vars preview.Vars, slot int, pidPath string) (previewResult, error) {
	command, err := preview.Render("start", a.cfg.Preview.Start, vars)
	if err != nil {
		return res, userErrorf("%v", err)
	}
	url, err := preview.Render("url", a.cfg.Preview.URL, vars)
	if err != nil {
		return res, userErrorf("%v", err)
	}
	spec.Command = command
	proc, err := preview.Start(spec, pidPath)
	if err != nil {
		if cerr := a.clearPreview(ref); cerr != nil {
			return res, cerr
		}
		res.Outcome, res.Detail = outFailed, fmt.Sprintf("starting %q: %v", command, err)
		return res, nil
	}
	p := &state.Preview{
		Mode: config.ModeStart, PID: proc.PID, URL: url, StartedAt: proc.StartedAt,
		Log: spec.Log, State: state.PreviewUp,
	}
	health := a.cfg.Preview.Health
	if health != "" {
		p.State = state.PreviewStarting
	}
	if err := a.setPreview(ref, slot, p); err != nil {
		return res, err
	}
	res.Slot, res.URL, res.Outcome = slot, url, outUp
	if health == "" {
		return res, nil
	}
	target := preview.HealthURL(url, health)
	timeout := a.cfg.Preview.HealthTimeout()
	a.infof("%s: waiting for %s (up to %s)", ref, target, timeout)
	if err := preview.WaitHealthy(a.ctx, target, timeout, healthInterval, proc.Exited()); err != nil {
		p.State, p.Error = state.PreviewFailed, err.Error()
		held := slot
		if !preview.Alive(p.PID, p.StartedAt) {
			p.PID, held = 0, 0 // nothing is running, so nothing occupies a slot
		}
		if serr := a.setPreview(ref, held, p); serr != nil {
			return res, serr
		}
		res.Outcome, res.Slot, res.URL = outFailed, 0, ""
		res.Detail = fmt.Sprintf("%v (log: %s)", err, spec.Log)
		return res, nil
	}
	p.State = state.PreviewUp
	return res, a.setPreview(ref, slot, p)
}

func (a *app) deployOne(ref state.Ref, res previewResult, spec preview.Spec, vars preview.Vars, slot int) (previewResult, error) {
	command, err := preview.Render("deploy", a.cfg.Preview.Deploy, vars)
	if err != nil {
		return res, userErrorf("%v", err)
	}
	spec.Command = command
	p := &state.Preview{Mode: config.ModeDeploy, StartedAt: time.Now().UTC(), Log: spec.Log, State: state.PreviewStarting}
	if err := a.setPreview(ref, slot, p); err != nil {
		return res, err
	}
	deadline := a.cfg.Preview.DeployDeadline()
	a.infof("%s: deploying as %s (up to %s)", ref, vars.Name, deadline)
	url, err := preview.Deploy(a.ctx, spec, deadline)
	if err != nil {
		// The environment may exist despite the failure, so the slot stays
		// taken until `down` has had its say; the log is kept either way.
		p.State, p.Error = state.PreviewFailed, err.Error()
		if serr := a.setPreview(ref, slot, p); serr != nil {
			return res, serr
		}
		res.Outcome, res.Slot = outFailed, slot
		res.Detail = fmt.Sprintf("%v (log: %s)", err, spec.Log)
		return res, nil
	}
	p.State, p.URL = state.PreviewUp, url
	res.Outcome, res.Slot, res.URL = outUp, slot, url
	return res, a.setPreview(ref, slot, p)
}

// acquireSlot reserves a slot for ref. When the budget is full the least
// recently started preview elsewhere is reclaimed; when there is nothing
// to reclaim the user has to free a slot themselves.
func (a *app) acquireSlot(ref state.Ref, mode string, mine map[state.Ref]bool) (int, error) {
	budget := a.cfg.Preview.Slots
	for attempt := 0; attempt < 2; attempt++ {
		st, err := a.load()
		if err != nil {
			return 0, err
		}
		slot, victim := pickSlot(st, budget, ref, mine)
		if slot == 0 {
			return 0, userErrorf("the preview budget of %d slot(s) is full; `bothways down <decision>=<variant>` frees one, or raise [preview] slots", budget)
		}
		if victim != nil {
			a.infof("%s: preview budget full; reclaiming slot %d from %s", ref, slot, *victim)
			if _, err := a.stopPreview(*victim); err != nil {
				return 0, err
			}
		}
		taken, err := a.reserveSlot(ref, slot, mode)
		if err != nil {
			return 0, err
		}
		if taken {
			return slot, nil
		}
	}
	return 0, partialErrorf("could not reserve a preview slot for %s: another bothways process took it", ref)
}

// pickSlot returns the slot ref should use and, when every slot was taken, the
// variant that has to give one up: previews that are not up go first, then the
// one started longest ago. A zero slot means nothing can be reclaimed.
func pickSlot(st *state.State, budget int, ref state.Ref, mine map[state.Ref]bool) (int, *state.Ref) {
	used := map[int]state.Ref{}
	for _, d := range st.Decisions {
		for _, v := range d.Variants {
			if v.HoldsSlot() && d.Ref(v) != ref {
				used[v.Slot] = d.Ref(v)
			}
		}
	}
	for slot := 1; slot <= budget; slot++ {
		if _, taken := used[slot]; !taken {
			return slot, nil
		}
	}
	victimSlot, rank, oldest := 0, 0, time.Time{}
	for slot := 1; slot <= budget; slot++ {
		holder := used[slot]
		if mine[holder] {
			continue
		}
		p := st.Variant(holder).Preview
		r := 1
		if p.State != state.PreviewUp {
			r = 0 // already broken: the cheapest slot to take back
		}
		if victimSlot == 0 || r < rank || (r == rank && p.StartedAt.Before(oldest)) {
			victimSlot, rank, oldest = slot, r, p.StartedAt
		}
	}
	if victimSlot == 0 {
		return 0, nil
	}
	victim := used[victimSlot]
	return victimSlot, &victim
}

// reserveSlot records ref as the owner of slot, re-checking under the state
// lock that no concurrent bothways took it first.
func (a *app) reserveSlot(ref state.Ref, slot int, mode string) (bool, error) {
	taken := false
	err := a.update(func(st *state.State) error {
		for _, d := range st.Decisions {
			for _, v := range d.Variants {
				if v.HoldsSlot() && v.Slot == slot && d.Ref(v) != ref {
					return nil
				}
			}
		}
		v := st.Variant(ref)
		if v == nil {
			return nil
		}
		v.Slot, v.Preview = slot, &state.Preview{Mode: mode, State: state.PreviewStarting}
		taken = true
		return nil
	})
	return taken, err
}

// stopPreview stops ref's preview and releases its slot. It reports whether
// there was anything to stop.
func (a *app) stopPreview(ref state.Ref) (bool, error) {
	st, err := a.load()
	if err != nil {
		return false, err
	}
	v := st.Variant(ref)
	if v == nil {
		return false, nil
	}
	p := v.Preview
	if p == nil || p.State == state.PreviewDown {
		if v.Slot != 0 {
			return false, a.clearPreview(ref)
		}
		return false, nil
	}
	logPath, pidPath, err := a.previewFiles(ref)
	if err != nil {
		return false, err
	}
	if p.Mode == config.ModeDeploy {
		if err := a.runDownScript(ref, v, logPath); err != nil {
			return false, err
		}
	} else if p.PID > 0 && preview.Alive(p.PID, p.StartedAt) {
		a.infof("%s: stopping preview (pid %d)", ref, p.PID)
		if err := preview.Stop(p.PID, stopGrace); err != nil {
			return false, partialErrorf("stopping the preview of %s: %v", ref, err)
		}
	}
	os.Remove(pidPath)
	return true, a.clearPreview(ref)
}

// runDownScript releases a deployed environment. With no down script
// configured, deploy mode only forgets the deployment.
func (a *app) runDownScript(ref state.Ref, v *state.Variant, logPath string) error {
	script := a.cfg.Preview.Down
	if script == "" {
		return nil
	}
	if err := a.requireTrust("run the down script"); err != nil {
		return err
	}
	vars := preview.Vars{
		Slot: v.Slot, Port: a.cfg.Preview.BasePort + v.Slot, Name: ref.Name(),
		Decision: ref.Decision, Variant: ref.Variant, Worktree: v.Worktree,
	}
	command, err := preview.Render("down", script, vars)
	if err != nil {
		return userErrorf("%v", err)
	}
	env, err := preview.RenderEnv(a.cfg.Preview.Env, vars)
	if err != nil {
		return userErrorf("%v", err)
	}
	// pick and drop run this as the worktree is being reclaimed, so fall back
	// to the main worktree when it has already gone.
	dir := v.Worktree
	if !dirExists(dir) {
		dir = a.mainRoot
	}
	a.infof("%s: running the down script", ref)
	spec := preview.Spec{Dir: dir, Command: command, Env: env, Log: logPath}
	if err := preview.Run(a.ctx, spec, a.cfg.Preview.DeployDeadline()); err != nil {
		return partialErrorf("down script for %s failed: %v (log: %s); the environment may still exist", ref, err, logPath)
	}
	return nil
}

// refreshPreviews marks previews whose process is gone as down and releases
// their slots: bothways exiting never kills a preview, so the recorded pid is
// only a claim until it is checked.
func (a *app) refreshPreviews() (*state.State, error) {
	st, err := a.load()
	if err != nil {
		return nil, err
	}
	var dead []state.Ref
	for _, d := range st.Decisions {
		for _, v := range d.Variants {
			if deadPreview(v) {
				dead = append(dead, d.Ref(v))
			}
		}
	}
	if len(dead) == 0 {
		return st, nil
	}
	err = a.update(func(st *state.State) error {
		for _, ref := range dead {
			if v := st.Variant(ref); v != nil && deadPreview(v) {
				v.Preview.State, v.Preview.PID, v.Preview.URL = state.PreviewDown, 0, ""
				v.Slot = 0
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return a.load()
}

// deadPreview reports a start-mode preview whose process is no longer there.
func deadPreview(v *state.Variant) bool {
	p := v.Preview
	return p != nil && p.Mode == config.ModeStart && p.State != state.PreviewDown &&
		!preview.Alive(p.PID, p.StartedAt)
}

func (a *app) clearPreview(ref state.Ref) error {
	return a.update(func(st *state.State) error {
		v := st.Variant(ref)
		if v == nil {
			return nil
		}
		v.Slot = 0
		if v.Preview != nil {
			v.Preview.State, v.Preview.PID = state.PreviewDown, 0
			v.Preview.URL, v.Preview.Error = "", ""
		}
		return nil
	})
}

func (a *app) setPreview(ref state.Ref, slot int, p *state.Preview) error {
	record := *p
	return a.update(func(st *state.State) error {
		if v := st.Variant(ref); v != nil {
			v.Slot, v.Preview = slot, &record
		}
		return nil
	})
}

// previewFiles returns the log and pid files of ref, next to state.json so
// that they are found from any worktree.
func (a *app) previewFiles(ref state.Ref) (logPath, pidPath string, err error) {
	dir := filepath.Join(a.store.Dir, "previews")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", envErrorf("creating %s: %v", dir, err)
	}
	return filepath.Join(dir, ref.Name()+".log"), filepath.Join(dir, ref.Name()+".pid"), nil
}

func (a *app) reportPreview(results []previewResult, asJSON bool, fatal error) error {
	if asJSON {
		if results == nil {
			results = []previewResult{}
		}
		if err := writeJSON(a.out, map[string]any{"schema_version": state.SchemaVersion, "results": results}); err != nil {
			return err
		}
	} else {
		for _, r := range results {
			fields := []string{r.Name, r.Outcome}
			if r.URL != "" {
				fields = append(fields, r.URL)
			}
			if r.Detail != "" {
				fields = append(fields, r.Detail)
			}
			fmt.Fprintln(a.out, strings.Join(fields, "\t"))
		}
	}
	if fatal != nil {
		return fatal
	}
	problems := 0
	for _, r := range results {
		if r.Outcome == outFailed || r.Outcome == outSkipped {
			problems++
		}
	}
	if problems > 0 {
		return partialErrorf("%d preview(s) need attention", problems)
	}
	return nil
}
