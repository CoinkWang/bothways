package cli

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/CoinkWang/bothways/internal/config"
)

// presetView is the JSON contract of preset list and preset show.
type presetView struct {
	Name string `json:"name"`
	// Detected is set when "auto" picked the preset from a lockfile.
	Detected    bool              `json:"detected"`
	Lockfiles   []string          `json:"lockfiles"`
	Setup       string            `json:"setup"`
	Fingerprint []string          `json:"fingerprint"`
	Paths       map[string]string `json:"paths"`
}

func newPresetView(name string, detected bool, w *config.Worktree) presetView {
	v := presetView{
		Name:        name,
		Detected:    detected,
		Lockfiles:   config.PresetTriggers(name),
		Setup:       w.Setup,
		Fingerprint: w.Fingerprint,
		Paths:       w.Paths,
	}
	if v.Lockfiles == nil {
		v.Lockfiles = []string{}
	}
	if v.Fingerprint == nil {
		v.Fingerprint = []string{}
	}
	if v.Paths == nil {
		v.Paths = map[string]string{}
	}
	return v
}

func newPresetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "preset",
		Short: "Inspect the built-in ecosystem presets",
		Long: `Presets are the ecosystem layer: a lockfile selects gitignored path strategies
and a setup command. Presets are opt-in because they run install commands:
[worktree] preset defaults to "none"; "auto" detects one from the lockfiles in
the repository root, and an explicit name wins. Whatever .bothways.toml sets itself always beats the
preset, which only fills in the rest.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return userErrorf("unknown preset command %q (try list or show)", args[0])
			}
			return cmd.Help()
		},
	}
	cmd.AddCommand(newPresetListCmd(), newPresetShowCmd())
	return cmd
}

func newPresetListCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the built-in ecosystem presets",
		Args:  wrapArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			views := []presetView{}
			for _, name := range config.PresetNames() {
				w, err := config.Preset(name)
				if err != nil {
					return envErrorf("%v", err)
				}
				views = append(views, newPresetView(name, false, w))
			}
			if asJSON {
				return writeJSON(out, views)
			}
			tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tLOCKFILES\tPATHS\tSETUP")
			for _, v := range views {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
					v.Name, strings.Join(v.Lockfiles, " "), pathsSummary(v.Paths), orDash(v.Setup))
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func newPresetShowCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "show [<name>]",
		Short: "Print one preset, or the [worktree] settings in effect in this repository",
		Args:  wrapArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				return showPreset(cmd, args[0], asJSON)
			}
			a, err := openApp(cmd)
			if err != nil {
				return err
			}
			// The config is already merged; ResolvePreset only names the source.
			name, detected, err := a.cfg.ResolvePreset(a.mainRoot)
			if err != nil {
				return userErrorf("config: %v", err)
			}
			view := newPresetView(name, detected, &a.cfg.Worktree)
			if asJSON {
				return writeJSON(a.out, view)
			}
			writeEffective(a.out, view)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func showPreset(cmd *cobra.Command, name string, asJSON bool) error {
	if asJSON {
		w, err := config.Preset(name)
		if err != nil {
			return userErrorf("%v", err)
		}
		return writeJSON(cmd.OutOrStdout(), newPresetView(name, false, w))
	}
	src, err := config.PresetSource(name)
	if err != nil {
		return userErrorf("%v", err)
	}
	_, err = cmd.OutOrStdout().Write(src)
	return err
}

// writeEffective prints the merged [worktree] block, so that what a fork will
// actually do can be pasted into .bothways.toml.
func writeEffective(w io.Writer, v presetView) {
	source := "none"
	if v.Name != "" {
		source = v.Name
		if v.Detected {
			source += " (detected)"
		}
	}
	fmt.Fprintf(w, "# preset: %s\n[worktree]\n", source)
	fmt.Fprintf(w, "setup       = %s\n", strconv.Quote(v.Setup))
	quoted := make([]string, 0, len(v.Fingerprint))
	for _, f := range v.Fingerprint {
		quoted = append(quoted, strconv.Quote(f))
	}
	fmt.Fprintf(w, "fingerprint = [%s]\n", strings.Join(quoted, ", "))
	if len(v.Paths) == 0 {
		return
	}
	fmt.Fprintln(w, "\n[worktree.paths]")
	for _, p := range sortedKeys(v.Paths) {
		fmt.Fprintf(w, "%s = %s\n", strconv.Quote(p), strconv.Quote(v.Paths[p]))
	}
}

func pathsSummary(paths map[string]string) string {
	pairs := make([]string, 0, len(paths))
	for _, p := range sortedKeys(paths) {
		pairs = append(pairs, p+"="+paths[p])
	}
	return orDash(strings.Join(pairs, " "))
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
