package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	cfg, err := Parse([]byte(`
[worktree]
root        = "../wt"
setup       = "pnpm install --frozen-lockfile"
fingerprint = ["pnpm-lock.yaml", "package.json"]

[worktree.paths]
"node_modules" = "clone"
".env"         = "copy"

[preview]
start = "pnpm dev --port {{.Port}}"

[fork]
similarity_threshold = 0.8
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Worktree.Setup != "pnpm install --frozen-lockfile" || len(cfg.Worktree.Fingerprint) != 2 {
		t.Errorf("worktree = %+v", cfg.Worktree)
	}
	rules := cfg.PathRules()
	if len(rules) != 2 || rules[0] != (PathRule{".env", "copy"}) {
		t.Errorf("rules = %+v", rules)
	}
	if cfg.Fork.SimilarityThreshold != 0.8 {
		t.Errorf("threshold = %v", cfg.Fork.SimilarityThreshold)
	}
	if cfg.Preview.Mode() != ModeStart || cfg.Preview.Slots != 4 || cfg.Preview.BasePort != 3000 {
		t.Errorf("preview = %+v", cfg.Preview)
	}
	if cfg.Preview.HealthTimeout() != 60*time.Second || cfg.Run.Jobs != 2 {
		t.Errorf("timeout = %v jobs = %d", cfg.Preview.HealthTimeout(), cfg.Run.Jobs)
	}
	if got := cfg.ResolveRoot("/repo/app"); got != "/repo/wt" {
		t.Errorf("ResolveRoot = %q", got)
	}
}

func TestDefaults(t *testing.T) {
	cfg, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Fork.SimilarityThreshold != 0.6 || cfg.ResolveRoot("/r") != "" {
		t.Errorf("defaults = %+v", cfg)
	}
}

func TestInvalid(t *testing.T) {
	for _, doc := range []string{
		"[worktree.paths]\n\"x\" = \"hardlink\"\n",
		"[worktree.paths]\n\"../x\" = \"copy\"\n",
		"[fork]\nsimilarity_threshold = 2\n",
		"[worktree\n",
	} {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("Parse(%q) accepted", doc)
		}
	}
}

func TestLoadLayering(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	os.MkdirAll(filepath.Join(home, "bothways"), 0o755)
	os.WriteFile(filepath.Join(home, "bothways", "config.toml"), []byte("[worktree]\nroot = \"/user/root\"\nsetup = \"user\"\n"), 0o644)
	repo := t.TempDir()
	os.WriteFile(filepath.Join(repo, FileName), []byte("[worktree]\nsetup = \"repo\"\n"), 0o644)
	cfg, err := Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Worktree.Root != "/user/root" || cfg.Worktree.Setup != "repo" {
		t.Errorf("layered = %+v", cfg.Worktree)
	}
}

func TestResolveRootIsAbsoluteWithRelativeHome(t *testing.T) {
	t.Setenv("HOME", "relative-home")
	t.Setenv("USERPROFILE", "relative-home")
	cfg := &Config{Worktree: Worktree{Root: "~/wt"}}
	if got := cfg.ResolveRoot("/repo"); !filepath.IsAbs(got) {
		t.Errorf("ResolveRoot = %q, want an absolute path", got)
	}
}

func TestLoadRepoCommands(t *testing.T) {
	user := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", user)
	if err := os.MkdirAll(filepath.Join(user, "bothways"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The user's own runner is theirs: it never needs trusting.
	if err := os.WriteFile(filepath.Join(user, "bothways", "config.toml"), []byte("[run]\nrunner = \"claude -p\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	repo := t.TempDir()
	cfg, err := Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Repo.Path != "" || len(cfg.Repo.Commands) != 0 {
		t.Errorf("no repository file: Repo = %+v", cfg.Repo)
	}

	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, FileName), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("[worktree]\nroot = \"../wt\"\n\n[preview]\nslots = 2\n")
	cfg, err = Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Repo.Path != filepath.Join(repo, FileName) || len(cfg.Repo.SHA256) != 64 || len(cfg.Repo.Commands) != 0 {
		t.Errorf("file that runs nothing: Repo = %+v", cfg.Repo)
	}
	plain := cfg.Repo.SHA256

	write("[worktree]\nsetup = \"make deps\"\n\n[preview]\nstart = \"make dev\"\n\n[preview.env]\nPORT = \"{{.Port}}\"\nDB = \"app\"\n")
	cfg, err = Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, c := range cfg.Repo.Commands {
		keys = append(keys, c.Key)
	}
	want := []string{"worktree.setup", "preview.start", "preview.env.DB", "preview.env.PORT"}
	if strings.Join(keys, " ") != strings.Join(want, " ") {
		t.Errorf("commands = %v, want %v", keys, want)
	}
	if cfg.Repo.SHA256 == plain {
		t.Error("digest did not change with the content")
	}
	if cfg.Run.Runner != "claude -p" {
		t.Errorf("user runner lost: %q", cfg.Run.Runner)
	}
}
