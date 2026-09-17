package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rogpeppe/go-internal/testscript"

	"github.com/CoinkWang/bothways/internal/cli"
)

func TestMain(m *testing.M) {
	testscript.Main(m, map[string]func(){
		"bothways": func() { os.Exit(cli.Main()) },
	})
}

func TestScripts(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	testscript.Run(t, testscript.Params{
		Dir: filepath.Join("..", "..", "testdata", "script"),
		Setup: func(env *testscript.Env) error {
			home := filepath.Join(env.WorkDir, "home")
			gitconfig := filepath.Join(home, ".gitconfig")
			if err := os.MkdirAll(home, 0o755); err != nil {
				return err
			}
			cfg := "[user]\n\tname = Test\n\temail = test@example.com\n[init]\n\tdefaultBranch = main\n[advice]\n\tdetachedHead = false\n"
			if err := os.WriteFile(gitconfig, []byte(cfg), 0o644); err != nil {
				return err
			}
			env.Setenv("HOME", home)
			env.Setenv("GIT_CONFIG_GLOBAL", gitconfig)
			env.Setenv("GIT_CONFIG_NOSYSTEM", "1")
			env.Setenv("XDG_CACHE_HOME", filepath.Join(env.WorkDir, "cache"))
			env.Setenv("XDG_CONFIG_HOME", filepath.Join(env.WorkDir, "config"))
			env.Setenv("XDG_STATE_HOME", filepath.Join(env.WorkDir, "state"))
			env.Setenv("NO_COLOR", "1")
			env.Setenv("LC_ALL", "C")
			env.Defer(reapPreviews(env.WorkDir))
			return nil
		},
		Cmds: map[string]func(ts *testscript.TestScript, neg bool, args []string){
			// exitcode N cmd args... runs cmd and asserts its exit status.
			"exitcode": func(ts *testscript.TestScript, neg bool, args []string) {
				if neg || len(args) < 2 {
					ts.Fatalf("usage: exitcode N command [args...]")
				}
				want, err := strconv.Atoi(args[0])
				if err != nil {
					ts.Fatalf("exitcode: %v", err)
				}
				got := 0
				if err := ts.Exec(args[1], args[2:]...); err != nil {
					var ee *exec.ExitError
					if !errors.As(err, &ee) {
						ts.Fatalf("exitcode: %v", err)
					}
					got = ee.ExitCode()
				}
				if got != want {
					ts.Fatalf("%s exited %d, want %d", args[1], got, want)
				}
			},
			// setenv-stdout VAR stores the previous command's trimmed stdout.
			"setenv-stdout": func(ts *testscript.TestScript, neg bool, args []string) {
				if neg || len(args) != 1 {
					ts.Fatalf("usage: setenv-stdout VAR")
				}
				ts.Setenv(args[0], strings.TrimSpace(ts.ReadFile("stdout")))
			},
			// waitfor FILE REGEXP polls until FILE matches REGEXP: the effect of
			// work bothways leaves running in the background, such as the
			// post-commit hook's sync.
			"waitfor": func(ts *testscript.TestScript, neg bool, args []string) {
				if neg || len(args) != 2 {
					ts.Fatalf("usage: waitfor file regexp")
				}
				re, err := regexp.Compile("(?m)" + args[1])
				ts.Check(err)
				path := ts.MkAbs(args[0])
				deadline := time.Now().Add(60 * time.Second)
				for {
					data, _ := os.ReadFile(path)
					if re.Match(data) {
						return
					}
					if time.Now().After(deadline) {
						ts.Fatalf("waitfor: %s did not match %q within 60s; it holds:\n%s", args[0], args[1], data)
					}
					time.Sleep(50 * time.Millisecond)
				}
			},
			// commit FILE CONTENT MESSAGE writes a file and commits it in cwd.
			"commit": func(ts *testscript.TestScript, neg bool, args []string) {
				if neg || len(args) != 3 {
					ts.Fatalf("usage: commit file content message")
				}
				path := ts.MkAbs(args[0])
				os.MkdirAll(filepath.Dir(path), 0o755)
				ts.Check(os.WriteFile(path, []byte(args[1]+"\n"), 0o644))
				ts.Check(ts.Exec("git", "add", "--", args[0]))
				ts.Check(ts.Exec("git", "commit", "-q", "-m", args[2]))
			},
		},
	})
}
