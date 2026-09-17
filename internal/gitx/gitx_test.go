package gitx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestAtLeast(t *testing.T) {
	min := [3]int{2, 38, 0}
	for v, want := range map[[3]int]bool{
		{2, 38, 0}: true, {2, 45, 2}: true, {3, 0, 0}: true,
		{2, 37, 9}: false, {1, 99, 99}: false,
	} {
		if got := AtLeast(v, min); got != want {
			t.Errorf("AtLeast(%v) = %v, want %v", v, got, want)
		}
	}
}

func TestEnvDropsLeakedVars(t *testing.T) {
	t.Setenv("GIT_DIR", "/elsewhere")
	t.Setenv("GIT_INDEX_FILE", "/elsewhere/index")
	t.Setenv("LC_ALL", "de_DE.UTF-8")
	env := Env("X=1")
	for _, kv := range env {
		if strings.HasPrefix(kv, "GIT_DIR=") || strings.HasPrefix(kv, "GIT_INDEX_FILE=") || kv == "LC_ALL=de_DE.UTF-8" {
			t.Errorf("leaked %s", kv)
		}
	}
	for _, want := range []string{"GIT_TERMINAL_PROMPT=0", "LC_ALL=C", "X=1"} {
		if !slices.Contains(env, want) {
			t.Errorf("missing %s", want)
		}
	}
}

func TestRepoHelpers(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	dir := t.TempDir()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(dir, "gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	repo := filepath.Join(dir, "repo")
	must := func(_ string, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(Run(ctx, dir, "init", "-q", "-b", "main", repo))
	must(Run(ctx, repo, "config", "user.name", "T"))
	must(Run(ctx, repo, "config", "user.email", "t@example.com"))
	os.WriteFile(filepath.Join(repo, "a"), []byte("a"), 0o644)
	must(Run(ctx, repo, "add", "a"))
	must(Run(ctx, repo, "commit", "-q", "-m", "one"))

	v, err := Version(ctx, repo)
	if err != nil || v[0] < 2 {
		t.Fatalf("Version = %v, %v", v, err)
	}
	if b, _ := CurrentBranch(ctx, repo); b != "main" {
		t.Errorf("CurrentBranch = %q", b)
	}
	tip, ok, err := BranchTip(ctx, repo, "main")
	if err != nil || !ok || len(tip) < 40 {
		t.Fatalf("BranchTip = %q %v %v", tip, ok, err)
	}
	if _, ok, _ := BranchTip(ctx, repo, "nope"); ok {
		t.Error("BranchTip(nope) exists")
	}
	if dirty, _ := Dirty(ctx, repo, false); dirty {
		t.Error("clean repo reported dirty")
	}
	os.WriteFile(filepath.Join(repo, "untracked"), []byte("x"), 0o644)
	if dirty, _ := Dirty(ctx, repo, false); dirty {
		t.Error("untracked file counted without includeUntracked")
	}
	if dirty, _ := Dirty(ctx, repo, true); !dirty {
		t.Error("untracked file not counted with includeUntracked")
	}

	must("", CreateBranch(ctx, repo, "bothways/d/v", "main"))
	if err := CreateBranch(ctx, repo, "bothways/d/v", "main"); err == nil {
		t.Error("CreateBranch overwrote an existing branch")
	}
	if err := UpdateRef(ctx, repo, "test", "refs/heads/bothways/d/v", tip, strings.Repeat("1", 40)); err == nil {
		t.Error("UpdateRef ignored a stale old value")
	}
	wtPath := filepath.Join(dir, "wt")
	must("", WorktreeAdd(ctx, repo, wtPath, "bothways/d/v", false))
	wts, err := Worktrees(ctx, repo)
	if err != nil || len(wts) != 2 || wts[1].Branch != "bothways/d/v" || wts[0].Branch != "main" {
		t.Fatalf("Worktrees = %+v, %v", wts, err)
	}
	refs, err := ListRefs(ctx, repo, "refs/heads/bothways/")
	if err != nil || len(refs) != 1 || refs[0].Name != "refs/heads/bothways/d/v" || refs[0].CommitterUnix == 0 {
		t.Fatalf("ListRefs = %+v, %v", refs, err)
	}
	if busy, err := RebaseInProgress(ctx, wtPath); err != nil || busy {
		t.Errorf("RebaseInProgress = %v, %v", busy, err)
	}
	if _, err := Run(ctx, repo, "rev-parse", "--verify", "nope"); ExitCode(err) <= 0 {
		t.Errorf("ExitCode = %d for %v", ExitCode(err), err)
	}
}
