package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CoinkWang/bothways/internal/state"
)

// The script test cannot start a preview, so the framed path is covered here.
func TestComparePage(t *testing.T) {
	view := &statusView{BaseBranch: "main"}
	dv := &decisionView{Decision: state.Decision{ID: "shape", Question: "<b>round</b> or square?"}}
	dv.Variants = []*variantView{
		{Variant: state.Variant{ID: "circle", State: state.Idle, Preview: &state.Preview{URL: "http://localhost:3001", State: state.PreviewUp}}, Name: "shape=circle", Default: true},
		{Variant: state.Variant{ID: "square", State: state.Idle, Preview: &state.Preview{URL: "javascript:alert(1)"}}, Name: "shape=square"},
		{Variant: state.Variant{ID: "oval", State: state.Deferred}, Name: "shape=oval"},
	}
	path := filepath.Join(t.TempDir(), "bothways", "compare-shape.html")
	if err := writeComparePage(path, comparePageOf(view, dv)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	page := string(data)

	for _, want := range []string{
		`<title>&lt;b&gt;round&lt;/b&gt; or square?</title>`,
		`<main class="grid" style="--variants: 3">`,
		`<iframe src="http://localhost:3001"`,
		// A running preview reports its preview state, as status does.
		`<span class="badge">up</span>`,
		`class="fallback card"`,
		`not an http(s) address`,
		`deferred`,
		`bothways pick shape=circle`,
		`bothways pick shape=square`,
		`bothways pick shape=oval`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	for _, bad := range []string{"<b>round", "ZgotmplZ", "bothways up shape=oval"} {
		if strings.Contains(page, bad) {
			t.Errorf("page contains %q", bad)
		}
	}
	if n := strings.Count(page, "<iframe"); n != 1 {
		t.Errorf("got %d iframes, want 1", n)
	}
}
