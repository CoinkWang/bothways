// Package preview runs what a variant's URL points at: a long-running process
// bothways supervises (start mode) or a one-shot script that prints a URL
// (deploy mode). It deals in commands, ports and pids; decisions, variants and
// slots stay with the caller.
package preview

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"
)

// Vars are the template variables every preview command, URL and environment
// value is rendered with.
type Vars struct {
	Slot     int
	Port     int
	Name     string
	Decision string
	Variant  string
	Worktree string
}

// Env returns the BOTHWAYS_* variables injected into preview commands.
func (v Vars) Env() []string {
	return []string{
		"BOTHWAYS_SLOT=" + strconv.Itoa(v.Slot),
		"BOTHWAYS_NAME=" + v.Name,
		"BOTHWAYS_WORKTREE=" + v.Worktree,
		"BOTHWAYS_DECISION=" + v.Decision,
		"BOTHWAYS_VARIANT=" + v.Variant,
	}
}

// Render expands one configured template; what names it in error messages.
func Render(what, tmpl string, v Vars) (string, error) {
	t, err := template.New(what).Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("[preview] %s: %w", what, err)
	}
	var buf strings.Builder
	if err := t.Execute(&buf, v); err != nil {
		return "", fmt.Errorf("[preview] %s: %w", what, err)
	}
	return buf.String(), nil
}

// RenderEnv renders [preview.env] plus the BOTHWAYS_* variables into KEY=value
// pairs, sorted so the environment a preview sees is stable.
func RenderEnv(env map[string]string, v Vars) ([]string, error) {
	names := make([]string, 0, len(env))
	for name := range env {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]string, 0, len(env)+5)
	for _, name := range names {
		val, err := Render("env."+name, env[name], v)
		if err != nil {
			return nil, err
		}
		out = append(out, name+"="+val)
	}
	return append(out, v.Env()...), nil
}

// Spec is one command to run in a variant's worktree.
type Spec struct {
	Dir     string
	Command string
	Env     []string // appended to bothways' own environment
	Log     string   // file the command's output goes to
}

// Process is a preview bothways started.
type Process struct {
	PID int
	// StartedAt is taken before the fork, so a later command can tell our
	// process from one that inherited a recycled pid.
	StartedAt time.Time
	exited    chan error
}

// Exited receives once the process ends. It is nil for a process this
// invocation did not start, and a receive on a nil channel blocks forever,
// which is what the callers below want.
func (p *Process) Exited() <-chan error { return p.exited }

// Start launches a long-running preview in its own process group. The child is
// deliberately not tied to a context: bothways exiting never kills a preview,
// so the only thing that stops it is Stop.
func Start(s Spec, pidFile string) (*Process, error) {
	log, err := openLog(s.Log)
	if err != nil {
		return nil, err
	}
	defer log.Close()
	name, args := shell(s.Command)
	cmd := exec.Command(name, args...)
	cmd.Dir = s.Dir
	cmd.Env = append(os.Environ(), s.Env...)
	cmd.Stdout, cmd.Stderr = log, log
	newProcessGroup(cmd)
	// Full precision: slot reclaim orders previews by start time, and two
	// started within the same second must still compare.
	p := &Process{StartedAt: time.Now().UTC(), exited: make(chan error, 1)}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p.PID = cmd.Process.Pid
	go func() { p.exited <- cmd.Wait() }()
	if pidFile != "" {
		if err := os.WriteFile(pidFile, []byte(strconv.Itoa(p.PID)+"\n"), 0o644); err != nil {
			return p, err
		}
	}
	return p, nil
}

// pidReuseSlack absorbs the one-second granularity of the age the OS reports.
const pidReuseSlack = 5 * time.Second

// Alive reports whether the process bothways started at startedAt is still
// running. A zero startedAt skips the pid-reuse check.
func Alive(pid int, startedAt time.Time) bool {
	if pid <= 0 || !running(pid) {
		return false
	}
	if age, ok := processAge(pid); ok && !startedAt.IsZero() {
		// Our process is at least as old as the moment we recorded; a much
		// younger one only inherited the pid.
		return time.Since(startedAt) <= age+pidReuseSlack
	}
	return true
}

// Stop signals the preview's process group, escalating to SIGKILL when it has
// not gone away within grace.
func Stop(pid int, grace time.Duration) error {
	if pid <= 0 {
		return nil
	}
	if err := terminateGroup(pid); err != nil && running(pid) {
		return err
	}
	if waitGone(pid, grace) {
		return nil
	}
	if err := killGroup(pid); err != nil && running(pid) {
		return err
	}
	if waitGone(pid, 2*time.Second) {
		return nil
	}
	return fmt.Errorf("process %d did not stop", pid)
}

func waitGone(pid int, d time.Duration) bool {
	for deadline := time.Now().Add(d); ; {
		if !running(pid) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// ErrExited reports a preview whose command ended while we waited for it.
var ErrExited = errors.New("the preview command exited")

// WaitHealthy polls target every interval until it answers 200, the process
// exits or timeout elapses. The preview is "starting" until this returns.
func WaitHealthy(ctx context.Context, target string, timeout, interval time.Duration, exited <-chan error) error {
	client := &http.Client{
		Timeout:       5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	deadline := time.Now().Add(timeout)
	last := "no answer yet"
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return fmt.Errorf("polling %s: %w", target, err)
		}
		resp, err := client.Do(req)
		if err != nil {
			last = err.Error()
		} else {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			last = resp.Status
		}
		select {
		case err := <-exited:
			return fmt.Errorf("%w: %s", ErrExited, exitReason(err))
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not answer 200 within %s (last: %s)", target, timeout, last)
		}
	}
}

// HealthURL appends the health path to the preview URL.
func HealthURL(base, health string) string {
	if health == "" {
		return base
	}
	return strings.TrimSuffix(base, "/") + "/" + strings.TrimPrefix(health, "/")
}

// Run executes a one-shot preview script (deploy or down), its output going to
// the log file.
func Run(ctx context.Context, s Spec, timeout time.Duration) error {
	return run(ctx, s, timeout, nil)
}

// Deploy runs the deploy script and returns the URL it printed.
func Deploy(ctx context.Context, s Spec, timeout time.Duration) (string, error) {
	var out bytes.Buffer
	if err := run(ctx, s, timeout, &out); err != nil {
		return "", err
	}
	return LastURL(out.String())
}

// ErrNoURL reports a deploy script whose last output line is not a URL.
var ErrNoURL = errors.New("the deploy script did not print a URL")

// LastURL parses the last non-empty stdout line as an http(s) URL.
func LastURL(out string) (string, error) {
	lines := strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		u, err := url.Parse(line)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return "", fmt.Errorf("%w: last line was %q", ErrNoURL, elide(line))
		}
		return line, nil
	}
	return "", fmt.Errorf("%w: it printed nothing", ErrNoURL)
}

func run(ctx context.Context, s Spec, timeout time.Duration, stdout io.Writer) error {
	log, err := openLog(s.Log)
	if err != nil {
		return err
	}
	defer log.Close()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	name, args := shell(s.Command)
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = s.Dir
	cmd.Env = append(os.Environ(), s.Env...)
	cmd.Stdout = io.Writer(log)
	if stdout != nil {
		cmd.Stdout = io.MultiWriter(log, stdout)
	}
	cmd.Stderr = log
	newProcessGroup(cmd)
	// The script is a shell: killing it alone would orphan what it spawned.
	cmd.Cancel = func() error { return terminateGroup(cmd.Process.Pid) }
	cmd.WaitDelay = 5 * time.Second
	err = cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("timed out after %s", timeout)
	}
	return err
}

func openLog(path string) (*os.File, error) {
	if path == "" {
		return os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
}

func exitReason(err error) string {
	if err == nil {
		return "exit status 0"
	}
	return err.Error()
}

func elide(s string) string {
	if len(s) > 80 {
		return s[:77] + "..."
	}
	return s
}
