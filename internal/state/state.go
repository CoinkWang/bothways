// Package state holds bothways' local, machine-specific state
// ($GIT_DIR/bothways/state.json). Durable facts (which variants exist, what
// they stack on) live in git branches; this file only adds what git cannot
// express: questions, defaults, recorded base tips, worktree paths, notes.
package state

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// SchemaVersion is bumped only for incompatible changes; fields are only ever added.
const SchemaVersion = 1

// BranchPrefix is the namespace of open variant branches.
const BranchPrefix = "bothways/"

// RejectedPrefix is the namespace rejected variant branches are renamed into.
const RejectedPrefix = "bothways/rejected/"

// Variant lifecycle states stored in (or computed for) Variant.State.
const (
	Idle     = "idle"
	Conflict = "conflict"
	Deferred = "deferred"
	Missing  = "missing" // computed only: worktree recorded but gone
	// Reclaimed marks an idle worktree gc removed to stay under max_disk. The
	// worktree is a cache: the next sync, up or doctor rebuilds it.
	Reclaimed = "reclaimed"
)

type State struct {
	SchemaVersion int         `json:"schema_version"`
	BaseBranch    string      `json:"base_branch"`
	Decisions     []*Decision `json:"decisions"`
	Combos        []Combo     `json:"combos"`
	Picked        []Picked    `json:"picked"`
	Rejected      []Rejected  `json:"rejected"`
}

type Decision struct {
	ID        string     `json:"id"`
	Question  string     `json:"question"`
	Default   string     `json:"default"`
	Under     *Ref       `json:"under" jsonschema:"nullable"`
	CreatedAt time.Time  `json:"created_at"`
	Variants  []*Variant `json:"variants"`
}

type Variant struct {
	ID      string `json:"id"`
	Branch  string `json:"branch"`
	BaseTip string `json:"base_tip"`
	// Worktree is empty while the variant is deferred.
	Worktree string   `json:"worktree"`
	Slot     int      `json:"slot,omitempty"`
	Preview  *Preview `json:"preview,omitempty"`
	Notes    []string `json:"notes,omitempty"`
	State    string   `json:"state"`
	// SyncOnto is the parent tip a conflicted rebase is heading to; once the
	// user finishes the rebase it becomes BaseTip.
	SyncOnto         string `json:"sync_onto,omitempty"`
	SetupFingerprint string `json:"setup_fingerprint,omitempty"`
}

// Preview is the process (start mode) or deployment (deploy mode) behind a
// variant's URL. A variant holds its Slot for exactly as long as this record
// is not PreviewDown.
type Preview struct {
	Mode string `json:"mode"`
	PID  int    `json:"pid,omitempty"`
	URL  string `json:"url,omitempty"`
	// StartedAt pairs with PID: a process that is younger than this only
	// inherited a recycled pid.
	StartedAt time.Time `json:"started_at,omitzero"`
	Log       string    `json:"log,omitempty"`
	State     string    `json:"state"`
	Error     string    `json:"error,omitempty"`
}

// Preview states.
const (
	PreviewUp       = "up"
	PreviewStarting = "starting" // health polling has not answered 200 yet
	PreviewDown     = "down"
	PreviewFailed   = "failed"
)

// HoldsSlot reports whether v currently occupies one of the preview budget's
// slots. Slots belong to running previews, not to worktrees.
func (v *Variant) HoldsSlot() bool {
	return v.Slot > 0 && v.Preview != nil && v.Preview.State != PreviewDown
}

// Combo is a read-only worktree merging several variants on top of the base.
// It is deliberately not a Variant and lives outside Decisions: nothing may
// be committed in it, so no command that walks decisions must ever pick it up
// as something to sync, pick or drop.
type Combo struct {
	Name     string `json:"name"`
	Variants []Ref  `json:"variants"`
	Worktree string `json:"worktree"`
	// BaseTip is the base commit the variants were merged onto; Commit is the
	// detached HEAD the merges produced.
	BaseTip   string    `json:"base_tip"`
	Commit    string    `json:"commit"`
	CreatedAt time.Time `json:"created_at"`
}

type Picked struct {
	Decision string    `json:"decision"`
	Variant  string    `json:"variant"`
	Tip      string    `json:"tip"`
	At       time.Time `json:"at"`
}

type Rejected struct {
	Decision string    `json:"decision"`
	Variant  string    `json:"variant"`
	Branch   string    `json:"branch"`
	At       time.Time `json:"at"`
}

// New returns an empty state.
func New() *State {
	return &State{SchemaVersion: SchemaVersion, Decisions: []*Decision{}, Combos: []Combo{}, Picked: []Picked{}, Rejected: []Rejected{}}
}

// Ref names a variant as <decision>=<variant>.
type Ref struct {
	Decision string `json:"decision"`
	Variant  string `json:"variant"`
}

func (r Ref) String() string { return r.Decision + "=" + r.Variant }

// Name is the slug used for environment names: shape-circle.
func (r Ref) Name() string { return r.Decision + "-" + r.Variant }

// Branch is the variant branch name: bothways/shape/circle.
func (r Ref) Branch() string { return BranchPrefix + r.Decision + "/" + r.Variant }

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ValidateID checks a decision or variant id.
func ValidateID(kind, id string) error {
	switch {
	case !idPattern.MatchString(id):
		return fmt.Errorf("invalid %s id %q: use letters, digits, '.', '_' or '-'", kind, id)
	case strings.HasSuffix(id, ".lock") || strings.Contains(id, ".."):
		return fmt.Errorf("invalid %s id %q", kind, id)
	case kind == "decision" && id == "rejected":
		return fmt.Errorf("decision id %q is reserved", id)
	}
	return nil
}

// ParseRef parses "<decision>=<variant>" strictly.
func ParseRef(s string) (Ref, error) {
	d, v, ok := strings.Cut(s, "=")
	if !ok || d == "" || v == "" {
		return Ref{}, fmt.Errorf("expected <decision>=<variant>, got %q", s)
	}
	return Ref{Decision: d, Variant: v}, nil
}

// Resolve parses s as a variant reference against open decisions. The
// "<decision>=" part may be omitted when the variant id is unique.
func (s *State) Resolve(arg string) (Ref, error) {
	if strings.Contains(arg, "=") {
		ref, err := ParseRef(arg)
		if err != nil {
			return Ref{}, err
		}
		d := s.Decision(ref.Decision)
		if d == nil {
			return Ref{}, fmt.Errorf("no open decision %q", ref.Decision)
		}
		if d.Variant(ref.Variant) == nil {
			return Ref{}, fmt.Errorf("decision %q has no variant %q", ref.Decision, ref.Variant)
		}
		return ref, nil
	}
	var found []Ref
	for _, d := range s.Decisions {
		if d.Variant(arg) != nil {
			found = append(found, Ref{Decision: d.ID, Variant: arg})
		}
	}
	switch len(found) {
	case 0:
		return Ref{}, fmt.Errorf("no open variant %q", arg)
	case 1:
		return found[0], nil
	default:
		names := make([]string, len(found))
		for i, r := range found {
			names[i] = r.String()
		}
		return Ref{}, fmt.Errorf("variant %q is ambiguous: %s", arg, strings.Join(names, ", "))
	}
}

// Decision returns the open decision with id, or nil.
func (s *State) Decision(id string) *Decision {
	for _, d := range s.Decisions {
		if d.ID == id {
			return d
		}
	}
	return nil
}

// Combo returns the recorded combo with name, or nil.
func (s *State) Combo(name string) *Combo {
	for i := range s.Combos {
		if s.Combos[i].Name == name {
			return &s.Combos[i]
		}
	}
	return nil
}

// RemoveCombo forgets the combo with name.
func (s *State) RemoveCombo(name string) {
	out := s.Combos[:0]
	for _, c := range s.Combos {
		if c.Name != name {
			out = append(out, c)
		}
	}
	s.Combos = out
}

// Variant returns the variant ref points to, or nil.
func (s *State) Variant(ref Ref) *Variant {
	if d := s.Decision(ref.Decision); d != nil {
		return d.Variant(ref.Variant)
	}
	return nil
}

// Variant returns the variant with id, or nil.
func (d *Decision) Variant(id string) *Variant {
	for _, v := range d.Variants {
		if v.ID == id {
			return v
		}
	}
	return nil
}

// Ref returns the reference of variant v of d.
func (d *Decision) Ref(v *Variant) Ref { return Ref{Decision: d.ID, Variant: v.ID} }

// Children returns decisions nested directly under ref.
func (s *State) Children(ref Ref) []*Decision {
	var out []*Decision
	for _, d := range s.Decisions {
		if d.Under != nil && *d.Under == ref {
			out = append(out, d)
		}
	}
	return out
}

// Subtree returns all decisions nested (transitively) under ref, parents first.
func (s *State) Subtree(ref Ref) []*Decision {
	var out []*Decision
	for _, d := range s.Children(ref) {
		out = append(out, d)
		for _, v := range d.Variants {
			out = append(out, s.Subtree(d.Ref(v))...)
		}
	}
	return out
}

// Ordered returns all open decisions with every parent before its children.
func (s *State) Ordered() []*Decision {
	var out []*Decision
	for _, d := range s.Decisions {
		if d.Under == nil || s.Variant(*d.Under) == nil {
			out = append(out, d)
			for _, v := range d.Variants {
				out = append(out, s.Subtree(d.Ref(v))...)
			}
		}
	}
	return out
}

// Materialized reports whether d's variants should have worktrees: every
// ancestor variant on the path to the base must be its decision's default.
func (s *State) Materialized(d *Decision) bool {
	for seen := 0; d.Under != nil; seen++ {
		parent := s.Decision(d.Under.Decision)
		if parent == nil || seen > len(s.Decisions) {
			return true
		}
		if parent.Default != d.Under.Variant {
			return false
		}
		d = parent
	}
	return true
}

// RemoveDecision deletes the open decision with id.
func (s *State) RemoveDecision(id string) {
	out := s.Decisions[:0]
	for _, d := range s.Decisions {
		if d.ID != id {
			out = append(out, d)
		}
	}
	s.Decisions = out
}

// RemoveVariant deletes variant id from d.
func (d *Decision) RemoveVariant(id string) {
	out := d.Variants[:0]
	for _, v := range d.Variants {
		if v.ID != id {
			out = append(out, v)
		}
	}
	d.Variants = out
}

// Normalize fills nil slices so JSON output is stable.
func (s *State) Normalize() {
	if s.Decisions == nil {
		s.Decisions = []*Decision{}
	}
	if s.Combos == nil {
		s.Combos = []Combo{}
	}
	if s.Picked == nil {
		s.Picked = []Picked{}
	}
	if s.Rejected == nil {
		s.Rejected = []Rejected{}
	}
	for _, d := range s.Decisions {
		if d.Variants == nil {
			d.Variants = []*Variant{}
		}
	}
}
