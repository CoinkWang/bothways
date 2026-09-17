// Package presets embeds the ecosystem presets (layer L2): one TOML file per
// ecosystem, shaped like the [worktree] section of .bothways.toml. They are
// data, not behaviour — a repository's own config always wins over them.
package presets

import (
	"embed"
	"sort"
	"strings"
)

//go:embed *.toml
var files embed.FS

// Names lists the embedded presets in alphabetical order.
func Names() []string {
	entries, err := files.ReadDir(".")
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, strings.TrimSuffix(e.Name(), ".toml"))
	}
	sort.Strings(names)
	return names
}

// File returns the raw TOML of one preset.
func File(name string) ([]byte, bool) {
	data, err := files.ReadFile(name + ".toml")
	return data, err == nil
}
