// Package config loads .bothways.toml (repository) layered over
// ~/.config/bothways/config.toml (user): [worktree], [fork], [preview] (with
// [preview.env]) and [run].
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// FileName is the repository config file at the main worktree root.
const FileName = ".bothways.toml"

// Path strategies for gitignored paths.
const (
	StrategyClone = "clone"
	StrategyLink  = "link"
	StrategyCopy  = "copy"
	StrategyFresh = "fresh"
)

type Config struct {
	Worktree Worktree `toml:"worktree"`
	Fork     Fork     `toml:"fork"`
	Preview  Preview  `toml:"preview"`
	Run      Run      `toml:"run"`

	// Repo describes the repository file Load read, so that the trust check
	// compares the exact bytes this configuration came from.
	Repo RepoFile `toml:"-"`
}

// RepoFile is the repository config file as Load found it.
type RepoFile struct {
	Path     string    // absolute path; "" when the repository has no config file
	SHA256   string    // hex digest of its content
	Commands []Command // what it makes bothways run; empty means nothing to trust
}

// Command is one key of the repository file that makes bothways run a shell
// command, or shapes the environment one runs in.
type Command struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	// From names where a value the file did not spell out came from, such as
	// the setup command of the preset it selects.
	From string `json:"from,omitempty"`
}

type Worktree struct {
	// Root replaces $XDG_CACHE_HOME/bothways/<repo-id>. Relative paths are
	// resolved against the repository root; "~/" expands to $HOME.
	Root string `toml:"root"`
	// Preset names an embedded ecosystem preset. "none" is the default: a
	// preset runs install commands, so detecting one from the lockfiles
	// ("auto") is something a repository opts into.
	Preset      string            `toml:"preset"`
	Setup       string            `toml:"setup"`
	Fingerprint []string          `toml:"fingerprint"`
	Paths       map[string]string `toml:"paths"`
	// MaxDisk is the budget gc reclaims idle and rejected worktrees down to,
	// written like "20GB". Empty means no budget.
	MaxDisk string `toml:"max_disk"`
}

type Fork struct {
	SimilarityThreshold float64 `toml:"similarity_threshold"`
}

// Preview modes. A repository configures start (bothways supervises a
// long-running process) or deploy (a script prints a URL), never both.
const (
	ModeNone   = "none"
	ModeStart  = "start"
	ModeDeploy = "deploy"
)

type Preview struct {
	Start  string `toml:"start"`
	Deploy string `toml:"deploy"`
	Down   string `toml:"down"`
	// URL templates the address of a start-mode preview; deploy mode reads the
	// URL from the last non-empty line the deploy script prints.
	URL string `toml:"url"`
	// Health is a path appended to URL and polled until it answers 200.
	Health   string `toml:"health"`
	Slots    int    `toml:"slots"`
	BasePort int    `toml:"base_port"`
	// Timeout bounds health polling; DeployTimeout bounds the deploy script.
	Timeout       string            `toml:"timeout"`
	DeployTimeout string            `toml:"deploy_timeout"`
	Env           map[string]string `toml:"env"`
}

// Mode reports which preview mode the repository configured.
func (p Preview) Mode() string {
	switch {
	case p.Deploy != "":
		return ModeDeploy
	case p.Start != "":
		return ModeStart
	}
	return ModeNone
}

// HealthTimeout and DeployDeadline return the validated durations.
func (p Preview) HealthTimeout() time.Duration { d, _ := time.ParseDuration(p.Timeout); return d }
func (p Preview) DeployDeadline() time.Duration {
	d, _ := time.ParseDuration(p.DeployTimeout)
	return d
}

type Run struct {
	Runner string `toml:"runner"`
	Jobs   int    `toml:"jobs"`
}

// PathRule is one [worktree.paths] entry.
type PathRule struct {
	Path     string
	Strategy string
}

// Default returns the built-in configuration.
func Default() *Config {
	return &Config{
		Worktree: Worktree{Preset: "none"},
		Fork:     Fork{SimilarityThreshold: 0.6},
		Preview: Preview{
			URL:           "http://localhost:{{.Port}}",
			Slots:         4,
			BasePort:      3000,
			Timeout:       "60s",
			DeployTimeout: "15m",
		},
		Run: Run{Jobs: 2},
	}
}

// UserFile returns the user-level config path.
func UserFile() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "bothways", "config.toml")
}

// Load reads the user config and then the repository config at repoRoot;
// keys present in the repository file win.
func Load(repoRoot string) (*Config, error) {
	cfg := Default()
	if f := UserFile(); f != "" {
		if _, err := decodeFile(f, cfg); err != nil {
			return nil, err
		}
	}
	var repoData []byte
	if repoRoot != "" {
		path := filepath.Join(repoRoot, FileName)
		data, err := decodeFile(path, cfg)
		if err != nil {
			return nil, err
		}
		if data != nil {
			sum := sha256.Sum256(data)
			cfg.Repo = RepoFile{Path: path, SHA256: hex.EncodeToString(sum[:])}
			repoData = data
		}
	}
	if err := cfg.applyPreset(repoRoot); err != nil {
		return nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if repoData != nil {
		commands, err := cfg.repoCommands(repoData, repoRoot)
		if err != nil {
			return nil, err
		}
		cfg.Repo.Commands = commands
	}
	return cfg, nil
}

// repoCommands lists the keys of the repository file that make bothways run
// something: shell commands, the preset that supplies one, and the environment
// they run with. The file alone is decoded, so keys from the user's own config
// never count against the repository.
func (c *Config) repoCommands(data []byte, repoRoot string) ([]Command, error) {
	var own Config
	if err := toml.NewDecoder(bytes.NewReader(data)).Decode(&own); err != nil {
		return nil, err
	}
	var out []Command
	add := func(key, value string) {
		if strings.TrimSpace(value) != "" {
			out = append(out, Command{Key: key, Value: value})
		}
	}
	if p := strings.TrimSpace(own.Worktree.Preset); p != "" && p != PresetNone {
		add("worktree.preset", p)
		if own.Worktree.Setup == "" {
			// The preset fills setup in unless a config file set it.
			if name, _, err := c.ResolvePreset(repoRoot); err == nil && name != "" {
				if preset, err := Preset(name); err == nil && preset.Setup != "" && preset.Setup == c.Worktree.Setup {
					out = append(out, Command{Key: "worktree.setup", Value: preset.Setup, From: "preset " + name})
				}
			}
		}
	}
	add("worktree.setup", own.Worktree.Setup)
	add("preview.start", own.Preview.Start)
	add("preview.deploy", own.Preview.Deploy)
	add("preview.down", own.Preview.Down)
	names := make([]string, 0, len(own.Preview.Env))
	for name := range own.Preview.Env {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		out = append(out, Command{Key: "preview.env." + name, Value: own.Preview.Env[name]})
	}
	add("run.runner", own.Run.Runner)
	return out, nil
}

// Parse decodes a single TOML document over the defaults.
func Parse(data []byte) (*Config, error) {
	cfg := Default()
	if err := toml.NewDecoder(bytes.NewReader(data)).Decode(cfg); err != nil {
		return nil, err
	}
	return cfg, cfg.validate()
}

// decodeFile decodes path over cfg and returns the bytes it read, nil when the
// file does not exist.
func decodeFile(path string, cfg *Config) ([]byte, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := toml.NewDecoder(bytes.NewReader(data)).Decode(cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return data, nil
}

func (c *Config) validate() error {
	for p, s := range c.Worktree.Paths {
		switch s {
		case StrategyClone, StrategyLink, StrategyCopy, StrategyFresh:
		default:
			return fmt.Errorf("[worktree.paths] %q: unknown strategy %q (want clone, link, copy or fresh)", p, s)
		}
		if filepath.IsAbs(p) || strings.HasPrefix(filepath.Clean(p), "..") {
			return fmt.Errorf("[worktree.paths] %q: path must be relative to the repository", p)
		}
	}
	if t := c.Fork.SimilarityThreshold; t < 0 || t > 1 {
		return fmt.Errorf("[fork] similarity_threshold %v must be between 0 and 1", t)
	}
	if c.Worktree.MaxDisk != "" {
		if _, err := ParseBytes(c.Worktree.MaxDisk); err != nil {
			return fmt.Errorf("[worktree] max_disk %q: %w", c.Worktree.MaxDisk, err)
		}
	}
	return c.validatePreview()
}

func (c *Config) validatePreview() error {
	p := &c.Preview
	if p.Start != "" && p.Deploy != "" {
		return errors.New("[preview] set either start or deploy, not both")
	}
	if p.Slots < 1 {
		return fmt.Errorf("[preview] slots %d must be at least 1", p.Slots)
	}
	if p.BasePort < 1 || p.BasePort+p.Slots > 65535 {
		return fmt.Errorf("[preview] base_port %d + slots %d leaves the port range", p.BasePort, p.Slots)
	}
	for name, field := range map[string]*string{"timeout": &p.Timeout, "deploy_timeout": &p.DeployTimeout} {
		d, err := time.ParseDuration(*field)
		if err != nil {
			return fmt.Errorf("[preview] %s %q: %w", name, *field, err)
		}
		if d <= 0 {
			return fmt.Errorf("[preview] %s %q must be positive", name, *field)
		}
	}
	if c.Run.Jobs < 1 {
		return fmt.Errorf("[run] jobs %d must be at least 1", c.Run.Jobs)
	}
	return nil
}

// ParseBytes reads a size like "20GB", "512MiB" or "1024" into bytes.
func ParseBytes(s string) (int64, error) {
	t := strings.TrimSpace(s)
	units := []struct {
		suffix string
		mult   int64
	}{
		{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"TiB", 1 << 40},
		{"KB", 1e3}, {"MB", 1e6}, {"GB", 1e9}, {"TB", 1e12},
		{"K", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12}, {"B", 1},
	}
	mult := int64(1)
	for _, u := range units {
		if rest, ok := cutSuffixFold(t, u.suffix); ok {
			t, mult = strings.TrimSpace(rest), u.mult
			break
		}
	}
	n, err := strconv.ParseFloat(t, 64)
	if err != nil {
		return 0, errors.New("want a size like 20GB")
	}
	if n < 0 {
		return 0, errors.New("must not be negative")
	}
	return int64(n * float64(mult)), nil
}

func cutSuffixFold(s, suffix string) (string, bool) {
	if len(s) >= len(suffix) && strings.EqualFold(s[len(s)-len(suffix):], suffix) {
		return s[:len(s)-len(suffix)], true
	}
	return s, false
}

// PathRules returns [worktree.paths] sorted by path.
func (c *Config) PathRules() []PathRule {
	rules := make([]PathRule, 0, len(c.Worktree.Paths))
	for p, s := range c.Worktree.Paths {
		rules = append(rules, PathRule{Path: p, Strategy: s})
	}
	sort.Slice(rules, func(i, j int) bool { return rules[i].Path < rules[j].Path })
	return rules
}

// ResolveRoot expands Worktree.Root; it returns "" when unset.
func (c *Config) ResolveRoot(repoRoot string) string {
	root := c.Worktree.Root
	switch {
	case root == "":
		return ""
	case root == "~" || strings.HasPrefix(root, "~/"):
		if home, err := os.UserHomeDir(); err == nil {
			root = filepath.Join(home, strings.TrimPrefix(root, "~"))
		}
	case !filepath.IsAbs(root):
		root = filepath.Join(repoRoot, root)
	}
	// A relative HOME would leak a relative path into status --json.
	if abs, err := filepath.Abs(root); err == nil {
		return abs
	}
	return filepath.Clean(root)
}
