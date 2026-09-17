package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/CoinkWang/bothways/internal/presets"
)

// Selectors for [worktree] preset, besides a preset's own name.
const (
	PresetAuto = "auto"
	PresetNone = "none"
)

// presetLockfiles maps each preset to the files whose presence in the
// repository root selects it. The slice order is the detection order:
// ecosystems that install by hardlinking out of a user-level cache come first,
// so a repository carrying both a pnpm and an npm lockfile gets the cheaper of
// the two.
var presetLockfiles = []struct {
	preset string
	files  []string
}{
	{"pnpm", []string{"pnpm-lock.yaml"}},
	{"bun", []string{"bun.lock", "bun.lockb"}},
	{"uv", []string{"uv.lock"}},
	{"cargo", []string{"Cargo.lock"}},
	{"npm", []string{"package-lock.json"}},
	{"pip", []string{"requirements.txt"}},
}

// PresetNames lists the embedded presets.
func PresetNames() []string { return presets.Names() }

// PresetSource returns the raw TOML of an embedded preset.
func PresetSource(name string) ([]byte, error) {
	data, ok := presets.File(name)
	if !ok {
		return nil, fmt.Errorf("unknown preset %q (try `bothways preset list`)", name)
	}
	return data, nil
}

// Preset decodes an embedded preset into a [worktree] block.
func Preset(name string) (*Worktree, error) {
	data, err := PresetSource(name)
	if err != nil {
		return nil, err
	}
	var w Worktree
	if err := toml.Unmarshal(data, &w); err != nil {
		return nil, fmt.Errorf("preset %s: %w", name, err)
	}
	return &w, nil
}

// PresetTriggers returns the lockfiles that select a preset under "auto".
func PresetTriggers(name string) []string {
	for _, e := range presetLockfiles {
		if e.preset == name {
			return e.files
		}
	}
	return nil
}

// DetectPreset names the preset the lockfiles in repoRoot select, or "".
func DetectPreset(repoRoot string) string {
	if repoRoot == "" {
		return ""
	}
	for _, e := range presetLockfiles {
		for _, f := range e.files {
			if _, err := os.Stat(filepath.Join(repoRoot, f)); err == nil {
				return e.preset
			}
		}
	}
	return ""
}

// ResolvePreset reports which preset applies to repoRoot: the configured name,
// or one detected from the lockfiles when [worktree] preset is "auto". The
// name is empty when no preset applies.
func (c *Config) ResolvePreset(repoRoot string) (name string, detected bool, err error) {
	switch p := strings.TrimSpace(c.Worktree.Preset); p {
	case "", PresetNone:
		return "", false, nil
	case PresetAuto:
		name = DetectPreset(repoRoot)
		return name, name != "", nil
	default:
		if _, err := PresetSource(p); err != nil {
			return "", false, fmt.Errorf("[worktree] %w", err)
		}
		return p, false, nil
	}
}

// applyPreset fills in what the config files left unset from the ecosystem
// preset. A configured key always wins: the preset is a default, not an
// override.
func (c *Config) applyPreset(repoRoot string) error {
	name, _, err := c.ResolvePreset(repoRoot)
	if err != nil || name == "" {
		return err
	}
	p, err := Preset(name)
	if err != nil {
		return err
	}
	if c.Worktree.Setup == "" {
		c.Worktree.Setup = p.Setup
	}
	if len(c.Worktree.Fingerprint) == 0 {
		c.Worktree.Fingerprint = p.Fingerprint
	}
	for path, strategy := range p.Paths {
		if _, ok := c.Worktree.Paths[path]; ok {
			continue
		}
		if c.Worktree.Paths == nil {
			c.Worktree.Paths = map[string]string{}
		}
		c.Worktree.Paths[path] = strategy
	}
	return nil
}
