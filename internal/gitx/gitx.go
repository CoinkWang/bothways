// Package gitx is the only place bothways talks to git. Every invocation
// passes an explicit -C <dir>, runs with a fixed environment
// (GIT_TERMINAL_PROMPT=0, LC_ALL=C) and never relies on the process cwd.
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// MinVersion is the oldest git bothways supports (rebase --update-refs).
var MinVersion = [3]int{2, 38, 0}

// leakedVars are variables a parent git process (e.g. a hook) may export that
// would silently redirect our -C invocations to another repository or index.
var leakedVars = map[string]bool{
	"GIT_DIR":                          true,
	"GIT_WORK_TREE":                    true,
	"GIT_INDEX_FILE":                   true,
	"GIT_PREFIX":                       true,
	"GIT_COMMON_DIR":                   true,
	"GIT_OBJECT_DIRECTORY":             true,
	"GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
	"GIT_NAMESPACE":                    true,
	"GIT_TERMINAL_PROMPT":              true,
	"LC_ALL":                           true,
}

// Env returns the environment every git invocation runs with, plus extra.
func Env(extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if leakedVars[name] {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	return append(env, extra...)
}

// Cmd describes one git invocation. Dir is mandatory.
type Cmd struct {
	Dir    string
	Args   []string
	Env    []string // appended to Env()
	Stdin  io.Reader
	Stdout io.Writer // when nil, output is captured and returned
	Stderr io.Writer // when nil, stderr is captured into *Error
}

// Error is returned when git exits non-zero.
type Error struct {
	Dir      string
	Args     []string
	ExitCode int
	Stderr   string
	Err      error
}

func (e *Error) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		msg = e.Err.Error()
	}
	return fmt.Sprintf("git %s: %s", strings.Join(e.Args, " "), msg)
}

func (e *Error) Unwrap() error { return e.Err }

// ExitCode reports git's exit code for err, or -1 if err is not a git exit.
func ExitCode(err error) int {
	var ge *Error
	if errors.As(err, &ge) {
		return ge.ExitCode
	}
	return -1
}

// ErrNotFound is returned when the git binary cannot be executed at all.
var ErrNotFound = errors.New("git executable not found")

// Exec runs c and returns captured stdout (untrimmed).
func Exec(ctx context.Context, c Cmd) (string, error) {
	if c.Dir == "" {
		return "", fmt.Errorf("gitx: missing -C dir for git %s", strings.Join(c.Args, " "))
	}
	args := append([]string{"-C", c.Dir}, c.Args...)
	if os.Getenv("BOTHWAYS_TRACE") != "" {
		fmt.Fprintf(os.Stderr, "+ git %s\n", strings.Join(args, " "))
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = Env(c.Env...)
	cmd.Stdin = c.Stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	if c.Stdout != nil {
		cmd.Stdout = c.Stdout
	}
	cmd.Stderr = &stderr
	if c.Stderr != nil {
		cmd.Stderr = io.MultiWriter(c.Stderr, &stderr)
	}
	err := cmd.Run()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return stdout.String(), &Error{Dir: c.Dir, Args: c.Args, ExitCode: ee.ExitCode(), Stderr: stderr.String(), Err: err}
		}
		if errors.Is(err, exec.ErrNotFound) {
			return "", ErrNotFound
		}
		return "", &Error{Dir: c.Dir, Args: c.Args, ExitCode: -1, Err: err}
	}
	return stdout.String(), nil
}

// Run runs git -C dir args... and returns stdout with the trailing newline removed.
func Run(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := Exec(ctx, Cmd{Dir: dir, Args: args})
	return strings.TrimRight(out, "\n"), err
}
