package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	validator "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/CoinkWang/bothways/internal/state"
)

// The committed schema is what SKILL.md points agents at, so it must never
// lag the structs it describes.
func TestSchemaUpToDate(t *testing.T) {
	want, err := StatusSchema()
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join("..", "..", SchemaPath))
	if err != nil {
		t.Fatalf("reading %s: %v; generate it with: %s", SchemaPath, err, SchemaRegenerate)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s does not match the structs status --json serializes; regenerate it with: %s", SchemaPath, SchemaRegenerate)
	}
}

// A real repository with an open decision, a nested deferred one, a conflict,
// a deployed preview, a pick and a combo must produce output the schema
// accepts, with the path and time conventions it promises.
func TestStatusJSONMatchesSchema(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the deploy script is sh")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	work := t.TempDir()
	home := filepath.Join(work, "home")
	gitconfig := filepath.Join(home, ".gitconfig")
	must(t, os.MkdirAll(home, 0o755))
	must(t, os.WriteFile(gitconfig, []byte("[user]\n\tname = Test\n\temail = test@example.com\n[init]\n\tdefaultBranch = main\n"), 0o644))
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", gitconfig)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("XDG_CACHE_HOME", filepath.Join(work, "cache"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(work, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(work, "state"))
	// A zone far from UTC makes any timestamp not converted to UTC visible.
	// time.Local is read once per process, so setting TZ would not do.
	local := time.Local
	time.Local = time.FixedZone("+0545", 5*3600+45*60)
	t.Cleanup(func() { time.Local = local })

	repo := filepath.Join(work, "repo")
	must(t, os.MkdirAll(repo, 0o755))
	t.Chdir(repo)
	git(t, repo, "init", "-q")
	// A relative worktree root is the likeliest way to leak a relative path.
	writeFile(t, repo, ".bothways.toml", "[worktree]\nroot = \"../wt\"\n\n[preview]\ndeploy = \"sh ./deploy.sh {{.Name}}\"\n")
	writeFile(t, repo, "deploy.sh", "echo \"https://$1.review.example.test\"\n")
	commitFile(t, repo, "style.css", "radius: 0", "base: style")

	bothways(t, 0, "trust")
	bothways(t, 0, "fork", "shape", "--alt", "circle", "--alt", "square", "--q", "round or square?")
	bothways(t, 0, "fork", "corner", "--under", "shape=square", "--alt", "sharp", "--alt", "soft")
	bothways(t, 0, "fork", "label", "--alt", "red", "--alt", "blue")
	bothways(t, 0, "pick", "label=red")
	bothways(t, 0, "fork", "color", "--alt", "warm", "--alt", "cool")
	bothways(t, 0, "note", "shape=square", "reviewer liked it")
	bothways(t, 0, "combo", "shape=square", "color=warm")
	bothways(t, 0, "up", "shape=square")

	circle := strings.TrimSpace(bothways(t, 0, "path", "shape=circle"))
	commitFile(t, circle, "style.css", "radius: 50%", "circle: radius")
	commitFile(t, repo, "style.css", "radius: 4px", "base: tweak radius")
	bothways(t, ExitPartial, "sync")

	out := bothways(t, 0, "status", "--json")
	schema := compileSchema(t)
	inst, err := validator.UnmarshalJSON(strings.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(inst); err != nil {
		t.Fatalf("status --json does not validate:\n%v\n%s", err, out)
	}

	var view statusView
	must(t, json.Unmarshal([]byte(out), &view))
	seen := map[string]bool{}
	for _, d := range view.Decisions {
		if d.Deferred && d.Under != nil {
			seen["deferred nested decision"] = true
		}
		for _, v := range d.Variants {
			seen["state "+v.State] = true
			if v.Preview != nil {
				seen["preview "+v.Preview.State] = true
			}
		}
	}
	if len(view.Picked) > 0 && len(view.Rejected) > 0 {
		seen["pick"] = true
	}
	if len(view.Combos) > 0 {
		seen["combo"] = true
	}
	for _, want := range []string{"deferred nested decision", "state conflict", "state idle", "state deferred", "preview up", "preview down", "pick", "combo"} {
		if !seen[want] {
			t.Errorf("scenario lacks %s:\n%s", want, out)
		}
	}

	// Paths are absolute and times are RFC 3339 UTC, checked on the raw
	// JSON rather than through time.Time, which would accept any offset.
	var raw any
	must(t, json.Unmarshal([]byte(out), &raw))
	walkJSON(raw, "", func(path string, value any) {
		s, ok := value.(string)
		if !ok {
			return
		}
		key := path[strings.LastIndex(path, ".")+1:]
		switch key {
		case "worktree", "log":
			if s != "" && !filepath.IsAbs(s) {
				t.Errorf("%s = %q is not absolute", path, s)
			}
		case "created_at", "started_at", "at":
			if _, err := time.Parse(time.RFC3339Nano, s); err != nil || !strings.HasSuffix(s, "Z") {
				t.Errorf("%s = %q is not RFC 3339 UTC", path, s)
			}
		}
	})
}

// The enums must constrain: a state the schema does not list is rejected.
func TestSchemaRejectsUnknownState(t *testing.T) {
	doc := `{"schema_version": 1, "base_branch": "main", "base_tip": "", "picked": [], "rejected": [], "combos": [], "hooks": {"post_commit": false, "binary_ok": false},
		"decisions": [{"id": "shape", "question": "", "default": "circle", "under": null, "created_at": "2026-01-02T03:04:05Z",
		"parent_branch": "main", "deferred": false, "variants": [{"id": "circle", "name": "shape=circle", "branch": "bothways/shape/circle",
		"base_tip": "", "worktree": "", "state": "exploded", "default": true, "tip": "", "ahead": 0, "behind_base": 0, "deferred": false}]}]}`
	inst, err := validator.UnmarshalJSON(strings.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	schema := compileSchema(t)
	if err := schema.Validate(inst); err == nil {
		t.Fatal("state \"exploded\" validated")
	}
	fixed, err := validator.UnmarshalJSON(strings.NewReader(strings.Replace(doc, "exploded", state.Idle, 1)))
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(fixed); err != nil {
		t.Fatalf("valid document rejected: %v", err)
	}
}

func compileSchema(t *testing.T) *validator.Schema {
	t.Helper()
	data, err := StatusSchema()
	if err != nil {
		t.Fatal(err)
	}
	doc, err := validator.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	c := validator.NewCompiler()
	c.AssertFormat()
	if err := c.AddResource(schemaID, doc); err != nil {
		t.Fatal(err)
	}
	schema, err := c.Compile(schemaID)
	if err != nil {
		t.Fatal(err)
	}
	return schema
}

func walkJSON(v any, path string, fn func(string, any)) {
	fn(path, v)
	switch v := v.(type) {
	case map[string]any:
		for k, child := range v {
			walkJSON(child, path+"."+k, fn)
		}
	case []any:
		for i, child := range v {
			walkJSON(child, path+"["+strconv.Itoa(i)+"]", fn)
		}
	}
}

func bothways(t *testing.T, want int, args ...string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if got := Run(context.Background(), args, &stdout, &stderr); got != want {
		t.Fatalf("bothways %s exited %d, want %d\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), got, want, stdout.String(), stderr.String())
	}
	return stdout.String()
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	must(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
}

func commitFile(t *testing.T, dir, name, content, message string) {
	t.Helper()
	writeFile(t, dir, name, content+"\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", message)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
