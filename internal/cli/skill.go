package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	embedded "github.com/CoinkWang/bothways"
)

func newSkillCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skill",
		Short: "Print or install the agent skill that matches this bothways",
		Long: `The binary embeds the SKILL.md written for it, so the skill an agent reads
never describes commands or flags this bothways does not have. Reinstall it
after upgrading bothways.

In Claude Code the plugin (` + "`/plugin marketplace add CoinkWang/bothways`" + `)
is the other way to get it; it follows the repository rather than the binary.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return userErrorf("unknown skill command %q (try show or install)", args[0])
			}
			return cmd.Help()
		},
	}
	cmd.AddCommand(newSkillShowCmd(), newSkillInstallCmd())
	return cmd
}

func newSkillShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Print the embedded SKILL.md",
		Args:  wrapArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := io.WriteString(cmd.OutOrStdout(), embedded.Skill)
			return err
		},
	}
}

func newSkillInstallCmd() *cobra.Command {
	var dir string
	var force bool
	cmd := &cobra.Command{
		Use:   "install [--dir <skills dir>] [--force]",
		Short: "Write the embedded SKILL.md to <skills dir>/bothways/SKILL.md",
		Long: `install writes the embedded skill to <skills dir>/bothways/SKILL.md, by default
under ~/.claude/skills, where Claude Code invokes it as /bothways. Point --dir
at another agent's skills directory to install it there.

A SKILL.md already there that differs, from an older bothways or edited by
hand, is left alone unless --force replaces it.`,
		Args: wrapArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if dir == "" {
				home, err := os.UserHomeDir()
				if err != nil {
					return envErrorf("finding the home directory: %v (pass --dir)", err)
				}
				dir = filepath.Join(home, ".claude", "skills")
			}
			abs, err := filepath.Abs(dir)
			if err != nil {
				return envErrorf("%v", err)
			}
			return installSkill(cmd.OutOrStdout(), filepath.Join(abs, "bothways", "SKILL.md"), force)
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "skills directory to install into (default ~/.claude/skills)")
	cmd.Flags().BoolVar(&force, "force", false, "replace a SKILL.md that differs")
	return cmd
}

func installSkill(out io.Writer, path string, force bool) error {
	want := []byte(embedded.Skill)
	verb := "installed"
	have, err := os.ReadFile(path)
	switch {
	case err == nil && bytes.Equal(have, want):
		fmt.Fprintf(out, "up to date\t%s\n", path)
		return nil
	case err == nil && !force:
		return userErrorf("%s differs from the skill of bothways %s; rerun with --force to replace it", path, version())
	case err == nil:
		verb = "replaced"
	case !errors.Is(err, fs.ErrNotExist):
		return envErrorf("reading %s: %v", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return envErrorf("%v", err)
	}
	if err := os.WriteFile(path, want, 0o644); err != nil {
		return envErrorf("%v", err)
	}
	fmt.Fprintf(out, "%s\t%s\n", verb, path)
	return nil
}
