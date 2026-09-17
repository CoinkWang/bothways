package state

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func sample() *State {
	st := New()
	st.BaseBranch = "main"
	st.Decisions = []*Decision{
		{ID: "stroke", Default: "yes", Under: &Ref{"shape", "circle"}, Variants: []*Variant{{ID: "yes"}, {ID: "no"}}},
		{ID: "shape", Default: "circle", Variants: []*Variant{{ID: "circle"}, {ID: "square"}}},
		{ID: "color", Default: "red", Variants: []*Variant{{ID: "red"}, {ID: "circle"}}},
		{ID: "width", Default: "thin", Under: &Ref{"shape", "square"}, Variants: []*Variant{{ID: "thin"}}},
		{ID: "dash", Default: "on", Under: &Ref{"width", "thin"}, Variants: []*Variant{{ID: "on"}}},
	}
	return st
}

func TestResolve(t *testing.T) {
	st := sample()
	tests := []struct {
		in      string
		want    Ref
		wantErr bool
	}{
		{"shape=square", Ref{"shape", "square"}, false},
		{"square", Ref{"shape", "square"}, false},
		{"circle", Ref{}, true}, // ambiguous
		{"nope", Ref{}, true},
		{"shape=nope", Ref{}, true},
		{"=x", Ref{}, true},
	}
	for _, tt := range tests {
		got, err := st.Resolve(tt.in)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("Resolve(%q) = %v, %v; want %v, err=%v", tt.in, got, err, tt.want, tt.wantErr)
		}
	}
}

func TestOrderedAndMaterialized(t *testing.T) {
	st := sample()
	var ids []string
	for _, d := range st.Ordered() {
		ids = append(ids, d.ID)
	}
	if got, want := fmt.Sprint(ids), "[shape stroke width dash color]"; got != want {
		t.Errorf("Ordered = %s, want %s", got, want)
	}
	want := map[string]bool{"shape": true, "stroke": true, "color": true, "width": false, "dash": false}
	for id, w := range want {
		if got := st.Materialized(st.Decision(id)); got != w {
			t.Errorf("Materialized(%s) = %v, want %v", id, got, w)
		}
	}
	var sub []string
	for _, d := range st.Subtree(Ref{"shape", "square"}) {
		sub = append(sub, d.ID)
	}
	if got := fmt.Sprint(sub); got != "[width dash]" {
		t.Errorf("Subtree = %s", got)
	}
}

func TestValidateID(t *testing.T) {
	for _, ok := range []string{"shape", "label-color", "v1.2", "A_b"} {
		if err := ValidateID("variant", ok); err != nil {
			t.Errorf("ValidateID(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-x", "a/b", "a b", "x.lock", "a..b", "a=b"} {
		if ValidateID("variant", bad) == nil {
			t.Errorf("ValidateID(%q) accepted", bad)
		}
	}
	if ValidateID("decision", "rejected") == nil {
		t.Error("decision id rejected accepted")
	}
}

func TestSimilarity(t *testing.T) {
	tests := []struct {
		a, b string
		min  float64
		max  float64
	}{
		{"Round or square logo frame?", "round or square logo frame", 1, 1},
		{"logo 外框圆还是方", "logo 外框是圆还是方？", 0.8, 1},
		{"logo 外框圆还是方", "标签用什么颜色", 0, 0.2},
		{"", "anything", 0, 0},
	}
	for _, tt := range tests {
		got := Similarity(tt.a, tt.b)
		if got < tt.min || got > tt.max {
			t.Errorf("Similarity(%q, %q) = %.2f, want in [%.2f, %.2f]", tt.a, tt.b, got, tt.min, tt.max)
		}
	}
}

func TestStoreConcurrentUpdates(t *testing.T) {
	store := &Store{Dir: t.TempDir()}
	if err := store.Update(func(st *State) error {
		st.Decisions = []*Decision{{ID: "d", Variants: []*Variant{{ID: "v"}}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	const n = 32
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := store.Update(func(st *State) error {
				v := st.Variant(Ref{"d", "v"})
				v.Notes = append(v.Notes, fmt.Sprint(i))
				return nil
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	st, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := len(st.Variant(Ref{"d", "v"}).Notes); got != n {
		t.Fatalf("notes = %d, want %d (lost updates)", got, n)
	}
}

func TestExclusiveWaitsForTheHolder(t *testing.T) {
	store := &Store{Dir: t.TempDir()}
	release, err := store.Exclusive("pid 1 sync", func(string) { t.Error("first holder waited") })
	if err != nil {
		t.Fatal(err)
	}
	waitedFor := make(chan string, 1)
	acquired := make(chan struct{})
	go func() {
		second, err := store.Exclusive("pid 2 pick", func(other string) { waitedFor <- other })
		if err != nil {
			t.Error(err)
			return
		}
		close(acquired)
		second()
	}()
	if got := <-waitedFor; got != "pid 1 sync" {
		t.Errorf("waiting saw holder %q, want %q", got, "pid 1 sync")
	}
	select {
	case <-acquired:
		t.Fatal("second operation ran while the first held the lock")
	case <-time.After(100 * time.Millisecond):
	}
	release()
	select {
	case <-acquired:
	case <-time.After(5 * time.Second):
		t.Fatal("second operation never acquired the lock after release")
	}
}
