package cli

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/CoinkWang/bothways/internal/state"
)

//go:embed compare.html
var compareHTML string

var compareTemplate = template.Must(template.New("compare").Parse(compareHTML))

// comparePage is everything the page shows: the template picks between fields,
// it never derives text of its own.
type comparePage struct {
	Decision  string
	Title     string
	Heading   string
	Base      string
	Count     int
	GridStyle template.CSS
	Generated string
	Variants  []comparePanel
}

type comparePanel struct {
	ID      string
	Name    string // shape=circle
	State   string
	Default bool
	URL     string
	// Reason and Up replace the frame when there is no preview URL.
	Reason string
	Up     string
	Pick   string
	Notes  []string
}

func newCompareCmd() *cobra.Command {
	var noOpen bool
	cmd := &cobra.Command{
		Use:   "compare <decision>",
		Short: "Open a side-by-side preview page for a decision",
		Long: `compare writes a local HTML page that puts every variant of a decision next to
its siblings -- one iframe per preview, the question as the title, and each
variant's pick command in the footer -- and opens it in the browser.

It only reads: it starts no previews and records no verdict.`,
		Args: wrapArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openApp(cmd)
			if err != nil {
				return err
			}
			view, _, err := a.buildView()
			if err != nil {
				return err
			}
			var dv *decisionView
			for _, d := range view.Decisions {
				if d.ID == args[0] {
					dv = d
					break
				}
			}
			if dv == nil {
				return userErrorf("no open decision %q", args[0])
			}
			path := a.comparePath(dv.ID)
			if err := writeComparePage(path, comparePageOf(view, dv)); err != nil {
				return envErrorf("writing %s: %v", path, err)
			}
			fmt.Fprintln(a.out, path)
			if noOpen {
				return nil
			}
			if err := openInBrowser(cmd.Context(), path); err != nil {
				a.warnf("could not open a browser (%v); open the page above yourself, or pass --no-open", err)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "write the page but do not launch a browser (over SSH, or in scripts)")
	return cmd
}

func comparePageOf(view *statusView, dv *decisionView) comparePage {
	page := comparePage{
		Decision:  dv.ID,
		Title:     dv.Question,
		Heading:   dv.Question,
		Base:      view.BaseBranch,
		Count:     len(dv.Variants),
		GridStyle: template.CSS(fmt.Sprintf("--variants: %d", len(dv.Variants))),
		Generated: time.Now().Format("2006-01-02 15:04 MST"),
	}
	if dv.Question == "" {
		page.Title, page.Heading = "bothways: "+dv.ID, "Which "+dv.ID+"?"
	}
	for _, v := range dv.Variants {
		p := comparePanel{
			ID: v.ID, Name: v.Name, State: displayState(v), Default: v.Default,
			Pick: "bothways pick " + v.Name, Notes: v.Notes,
		}
		addr := ""
		if v.Preview != nil {
			addr = v.Preview.URL
		}
		switch {
		case webURL(addr):
			p.URL = addr
		case addr != "":
			// Deploy mode takes the URL from a script's output. html/template
			// would defuse anything else to "#ZgotmplZ", which frames this page
			// inside itself.
			p.Reason = fmt.Sprintf("Its preview reported %q, which is not an http(s) address.", addr)
		case v.State == state.Deferred:
			p.Reason = "This variant is deferred: its parent variant is not the default, so it has no worktree to run yet."
		case v.State == state.Missing:
			p.Reason = "Its worktree is gone. Run bothways doctor to recreate it, then start a preview."
		case v.State == state.Reclaimed:
			p.Reason = "gc reclaimed its worktree to save disk. Starting it rebuilds the worktree first."
			p.Up = "bothways up " + v.Name
		default:
			p.Reason = "Start it, then run compare again."
			p.Up = "bothways up " + v.Name
		}
		page.Variants = append(page.Variants, p)
	}
	return page
}

func webURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// writeComparePage renders first so a template failure cannot truncate the
// page someone is currently looking at.
func writeComparePage(path string, page comparePage) error {
	var buf bytes.Buffer
	if err := compareTemplate.Execute(&buf, page); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

// comparePath is $GIT_DIR/bothways/compare/<decision>.html, beside the
// preview pid and log files and the run prompts: every per-machine artifact of
// a repository lives in one directory that every worktree already agrees on.
// The page is regenerated output, not state: overwriting it is the point, so
// the browser can be reloaded on the same URL.
func (a *app) comparePath(decision string) string {
	return filepath.Join(a.store.Dir, "compare", decision+".html")
}

func openInBrowser(ctx context.Context, path string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "open", path)
	case "windows":
		cmd = exec.CommandContext(ctx, "cmd", "/C", "start", "", path)
	default:
		cmd = exec.CommandContext(ctx, "xdg-open", path)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		if msg := string(bytes.TrimSpace(out)); msg != "" {
			return fmt.Errorf("%s: %s", err, msg)
		}
		return err
	}
	return nil
}
