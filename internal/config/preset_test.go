package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestDetectPreset(t *testing.T) {
	tests := []struct {
		name  string
		files []string
		want  string
	}{
		{"pnpm", []string{"pnpm-lock.yaml"}, "pnpm"},
		{"bun", []string{"bun.lockb", "package.json"}, "bun"},
		{"uv", []string{"uv.lock"}, "uv"},
		{"cargo", []string{"Cargo.lock"}, "cargo"},
		{"npm", []string{"package-lock.json"}, "npm"},
		{"pip", []string{"requirements.txt"}, "pip"},
		{"hardlinking ecosystem wins", []string{"pnpm-lock.yaml", "package-lock.json"}, "pnpm"},
		{"manifest without a lockfile", []string{"package.json"}, ""},
		{"nothing", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, f := range tt.files {
				if err := os.WriteFile(filepath.Join(dir, f), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := DetectPreset(dir); got != tt.want {
				t.Errorf("DetectPreset = %q, want %q", got, tt.want)
			}
		})
	}
}

// Every shipped preset must parse and pass the same validation as a hand
// written [worktree] block.
func TestPresetsAreValid(t *testing.T) {
	names := PresetNames()
	for _, want := range []string{"pnpm", "bun", "uv", "cargo", "npm", "pip"} {
		if !slices.Contains(names, want) {
			t.Errorf("preset %q is missing", want)
		}
	}
	for _, name := range names {
		w, err := Preset(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		cfg := Default()
		cfg.Worktree = *w
		if err := cfg.validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if w.Setup == "" || len(w.Fingerprint) == 0 || len(w.Paths) == 0 {
			t.Errorf("%s: incomplete preset %+v", name, w)
		}
		if len(PresetTriggers(name)) == 0 {
			t.Errorf("%s: no lockfile selects it under auto", name)
		}
	}
}

func TestLoadAppliesPreset(t *testing.T) {
	tests := []struct {
		name            string
		lockfile        string
		repo            string
		wantPreset      string
		wantSetup       string
		wantNodeModules string
	}{
		{
			name:       "a lockfile alone selects nothing",
			lockfile:   "package-lock.json",
			wantPreset: "",
		},
		{
			name:            "auto detects from the lockfile",
			lockfile:        "package-lock.json",
			repo:            "[worktree]\npreset = \"auto\"\n",
			wantPreset:      "npm",
			wantSetup:       "npm install",
			wantNodeModules: "clone",
		},
		{
			name:            "the repository overrides what it sets",
			lockfile:        "pnpm-lock.yaml",
			repo:            "[worktree]\npreset = \"auto\"\nsetup = \"make deps\"\n\n[worktree.paths]\n\"node_modules\" = \"link\"\n",
			wantPreset:      "pnpm",
			wantSetup:       "make deps",
			wantNodeModules: "link",
		},
		{
			name:            "an explicit name beats detection",
			lockfile:        "pnpm-lock.yaml",
			repo:            "[worktree]\npreset = \"cargo\"\n",
			wantPreset:      "cargo",
			wantSetup:       "cargo fetch --locked",
			wantNodeModules: "",
		},
		{
			name:       "none disables detection",
			lockfile:   "pnpm-lock.yaml",
			repo:       "[worktree]\npreset = \"none\"\n",
			wantPreset: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repoRoot := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // ignore the developer's own config
			if err := os.WriteFile(filepath.Join(repoRoot, tt.lockfile), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			if tt.repo != "" {
				if err := os.WriteFile(filepath.Join(repoRoot, FileName), []byte(tt.repo), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			cfg, err := Load(repoRoot)
			if err != nil {
				t.Fatal(err)
			}
			if name, _, _ := cfg.ResolvePreset(repoRoot); name != tt.wantPreset {
				t.Errorf("preset = %q, want %q", name, tt.wantPreset)
			}
			if cfg.Worktree.Setup != tt.wantSetup {
				t.Errorf("setup = %q, want %q", cfg.Worktree.Setup, tt.wantSetup)
			}
			if got := cfg.Worktree.Paths["node_modules"]; got != tt.wantNodeModules {
				t.Errorf("node_modules = %q, want %q", got, tt.wantNodeModules)
			}
			// The preset fills in the fingerprint the repository left unset.
			if tt.wantPreset != "" && len(cfg.Worktree.Fingerprint) == 0 {
				t.Error("fingerprint was not filled in from the preset")
			}
			if tt.wantPreset == "" && cfg.Worktree.Setup != "" {
				t.Errorf("setup = %q with preset none", cfg.Worktree.Setup)
			}
		})
	}
}

func TestLoadRejectsUnknownPreset(t *testing.T) {
	repoRoot := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	os.WriteFile(filepath.Join(repoRoot, FileName), []byte("[worktree]\npreset = \"poetry\"\n"), 0o644)
	_, err := Load(repoRoot)
	if err == nil || !strings.Contains(err.Error(), "unknown preset") {
		t.Errorf("Load error = %v, want an unknown preset error", err)
	}
}
