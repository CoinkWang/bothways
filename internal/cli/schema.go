package cli

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/invopop/jsonschema"

	"github.com/CoinkWang/bothways/internal/config"
	"github.com/CoinkWang/bothways/internal/state"
)

//go:generate go run ../../cmd/schemagen ../../schema/state.schema.json

// SchemaPath is where the generated schema of `status --json` is committed,
// relative to the repository root.
const SchemaPath = "schema/state.schema.json"

// SchemaRegenerate is the one command that rewrites SchemaPath.
const SchemaRegenerate = "go generate ./internal/cli"

const schemaID = "https://github.com/CoinkWang/bothways/" + SchemaPath

// schemaDoc is what an agent reading the schema cold needs and cannot guess
// from a field name. Keys are <definition>.<property>, with an empty
// definition for the root; a key that no longer matches a property fails
// generation, so a rename cannot silently drop its description.
var schemaDoc = map[string]string{
	".schema_version": "Version of this contract; a reader built for this schema must refuse any other value.",
	".base_branch":    "The branch top-level decisions fork from and pick fast-forwards.",
	".base_tip":       "Commit base_branch points at now; empty if the branch does not exist yet.",
	".decisions":      "Open decisions, every parent listed before the decisions nested under its variants.",
	".picked":         "Decisions already settled by pick, oldest first.",
	".rejected":       "Variants that lost a pick or were dropped; their branches stay under bothways/rejected/ until gc removes them, so a reversed decision is recoverable.",
	".hooks": "Whether `bothways init --hooks` installed the post-commit hook. While it is installed, a commit on " +
		"base_branch starts sync in the background, so variants restack without anyone running sync.",
	"Hooks.post_commit": "True when the bothways post-commit hook is installed.",
	"Hooks.path":        "Absolute path of the installed hook file.",
	"Hooks.binary":      "Absolute path of the bothways binary the hook runs.",
	"Hooks.binary_ok":   "False when that binary is gone: the hook then only logs that it skipped. Rerun `bothways init --hooks` to record the current one.",
	"Hooks.chained":     "A post-commit hook that existed before install, kept and run after ours; restored by `bothways init --no-hooks`.",
	"Hooks.log":         "Absolute path of the log hook-triggered syncs write to.",
	".combos": "Read-only worktrees merging several variants on top of the base, for trying them together. " +
		"Combos are not variants: nothing may be committed in them, and a combo name is never an argument to sync, pick or drop.",

	"Decision.id":       "Decision id, the part before '=' in a variant name.",
	"Decision.question": "The question the variants answer; may be empty.",
	"Decision.default": "Id of the variant whose nested decisions get worktrees. Nothing is ever picked " +
		"automatically: the default only decides which branch of the tree is materialized. " +
		"Dropping the default makes the first remaining variant the default.",
	"Decision.under":      "The variant this decision is stacked on, or null when it forks from base_branch.",
	"Decision.created_at": "When the decision was forked, RFC 3339 in UTC.",
	"Decision.parent_branch": "The branch sync rebases this decision's variants onto: base_branch, " +
		"or the branch of the variant named in under.",
	"Decision.deferred": "True when some ancestor variant is not its decision's default: " +
		"the variants exist as branches but have no worktrees until that ancestor is picked " +
		"or becomes the default.",

	"Variant.id":     "Variant id, unique within its decision.",
	"Variant.name":   "<decision>=<variant>: how every command addresses this variant.",
	"Variant.branch": "The variant's git branch, bothways/<decision>/<variant>.",
	"Variant.default": "True for its decision's default variant (shown as * in status). " +
		"Only the default variant's nested decisions have worktrees; it is never picked automatically.",
	"Variant.base_tip": "Commit of the decision's parent_branch this variant was last forked or synced onto.",
	"Variant.tip":      "Commit the variant branch points at now; empty if the branch is gone, and state is then missing.",
	"Variant.ahead":    "Commits on the variant since base_tip: the work that makes it different.",
	"Variant.behind_base": "Commits the decision's parent_branch gained since base_tip. " +
		"Greater than 0 means the variant is stale and needs `bothways sync` (pick syncs on its own).",
	"Variant.worktree": "Absolute path of the worktree where the variant is edited and committed; " +
		"empty while the variant is deferred or reclaimed.",
	"Variant.slot":  "Preview budget slot the variant holds while its preview is not down; absent otherwise.",
	"Variant.notes": "Notes recorded with `bothways note`, oldest first.",
	"Variant.state": "Lifecycle state. " +
		"idle: the worktree exists and nothing is in progress (a running preview shows in preview.state). " +
		"conflict: a sync stopped mid-rebase in the worktree; finish the rebase there (git add, git rebase --continue) and the next command reports idle; pick refuses the variant until then. " +
		"deferred: nested under a non-default variant, so there is no worktree (see the decision's deferred). " +
		"missing: the recorded worktree directory or the branch is gone; `bothways doctor` repairs the record. " +
		"reclaimed: gc removed the idle worktree to stay under max_disk; the branch is intact and `bothways sync` rebuilds the worktree.",
	"Variant.sync_onto":         "Present only in conflict: the parent commit the stopped rebase is heading to; it becomes base_tip once the rebase is finished.",
	"Variant.setup_fingerprint": "Internal: identifies the setup last run in the worktree, so it reruns when the setup changes.",
	"Variant.deferred":          "Same as state == \"deferred\".",
	"Variant.preview":           "The preview behind the variant's URL as it is right now: a recorded process that has died reads as down.",

	"Preview.mode": "How the repository previews variants: start (bothways supervises a local server per slot), " +
		"deploy (a script deploys an environment and prints its URL), or none (no preview configured).",
	"Preview.pid":        "Process id, and process group, of a start-mode preview.",
	"Preview.url":        "Where the preview answers; absent while down.",
	"Preview.started_at": "When the preview was last started or deployed, RFC 3339 in UTC.",
	"Preview.log":        "Absolute path of the preview's log file; read it when state is failed.",
	"Preview.state": "up: the URL is serving (start mode passed its health check, or the deploy script printed the URL). " +
		"starting: the server or deploy script runs but has not answered yet. " +
		"down: no preview, and the slot is free. " +
		"failed: the health check or deploy script failed; error says why and log has the output.",
	"Preview.error": "Why the preview failed; present only when state is failed.",

	"Combo.name":       "Combo name, as `bothways combo` and gc report it; never a variant argument.",
	"Combo.variants":   "The variants merged, in merge order.",
	"Combo.worktree":   "Absolute path of the read-only, detached worktree.",
	"Combo.base_tip":   "Base commit the variants were merged onto.",
	"Combo.commit":     "Detached HEAD the merges produced.",
	"Combo.created_at": "When the combo was built, RFC 3339 in UTC.",

	"Picked.tip": "Commit base_branch was fast-forwarded to.",
	"Picked.at":  "When the pick happened, RFC 3339 in UTC.",

	"Rejected.branch": "Where the variant's branch was renamed to, under bothways/rejected/.",
	"Rejected.at":     "When the variant was rejected, RFC 3339 in UTC.",
}

// schemaEnums are the closed value sets, read from the constants the code
// itself switches on.
var schemaEnums = map[string][]any{
	"Variant.state": {state.Idle, state.Conflict, state.Deferred, state.Missing, state.Reclaimed},
	"Preview.state": {state.PreviewUp, state.PreviewStarting, state.PreviewDown, state.PreviewFailed},
	"Preview.mode":  {config.ModeStart, config.ModeDeploy, config.ModeNone},
}

// schemaNames exposes the view types under the names an agent knows them by.
// state.Variant is reachable only through state.Decision's variants, which
// decisionView shadows; it gets a name of its own so it cannot claim the
// "Variant" definition, and is dropped from the output.
var schemaNames = map[reflect.Type]string{
	reflect.TypeOf(decisionView{}):  "Decision",
	reflect.TypeOf(variantView{}):   "Variant",
	reflect.TypeOf(hooksView{}):     "Hooks",
	reflect.TypeOf(state.Variant{}): "shadowedVariant",
}

// StatusSchema returns the JSON Schema of `status --json`, generated from the
// structs it serializes, as committed at SchemaPath.
func StatusSchema() ([]byte, error) {
	r := &jsonschema.Reflector{
		// Fields are only ever added: a reader validating newer output
		// against this schema must not reject what it does not know yet.
		AllowAdditionalProperties: true,
		ExpandedStruct:            true,
		Namer:                     func(t reflect.Type) string { return schemaNames[t] },
	}
	s := r.Reflect(&statusView{})
	delete(s.Definitions, "shadowedVariant")
	s.ID = schemaID
	s.Title = fmt.Sprintf("bothways status --json, schema_version %d", state.SchemaVersion)
	s.Description = "Output of `bothways status --json`: the one JSON view of bothways state, shared by the CLI, the compare page and SKILL.md. " +
		"Fields are only ever added, so ignore the ones you do not know; schema_version changes only with an incompatible change. " +
		"Paths are absolute, or empty when there is none; times are RFC 3339 in UTC; commits are full hex object names."

	for key, doc := range schemaDoc {
		p, err := schemaProperty(s, key)
		if err != nil {
			return nil, err
		}
		p.Description = doc
	}
	for key, values := range schemaEnums {
		p, err := schemaProperty(s, key)
		if err != nil {
			return nil, err
		}
		p.Enum = values
	}
	version, err := schemaProperty(s, ".schema_version")
	if err != nil {
		return nil, err
	}
	version.Const = state.SchemaVersion

	out, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, err
	}
	// Schema's own MarshalJSON escapes HTML regardless of the encoder, and
	// descriptions quote <decision>=<variant>; the file is read, not embedded.
	out = []byte(strings.NewReplacer(`\u003c`, "<", `\u003e`, ">", `\u0026`, "&").Replace(string(out)))
	return append(out, '\n'), nil
}

// schemaProperty finds the property a schemaDoc key names.
func schemaProperty(s *jsonschema.Schema, key string) (*jsonschema.Schema, error) {
	def, name, _ := strings.Cut(key, ".")
	owner := s
	if def != "" {
		owner = s.Definitions[def]
	}
	if owner == nil || owner.Properties == nil {
		return nil, fmt.Errorf("schema: %s: no definition %q", key, def)
	}
	p, ok := owner.Properties.Get(name)
	if !ok {
		return nil, fmt.Errorf("schema: %s: no property %q", key, name)
	}
	return p, nil
}
