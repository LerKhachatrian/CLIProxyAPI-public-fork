package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFamilySteadyBoundSelectionHasNoStateIO(t *testing.T) {
	f := newFamilyFixture(t, 1, FamilyRoutingConfig{})
	_, opts := f.pick(t, "root", "child", "root", "model")
	lease, err := f.router.acquire(t.Context(), familyTokenFromOptions(opts))
	if err != nil {
		t.Fatal(err)
	}
	lease.release()
	f.router.mu.Lock()
	sequence := f.router.sequence
	f.router.mu.Unlock()
	// Mutate only this synthetic test file after the initial durable admission.
	// A generation-time read would fail and an unexpected write would erase it.
	data := []byte("synthetic evidence that steady selection must not read or replace")
	if err := os.WriteFile(f.router.cfg.StatePath, data, 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		_, opts = f.pick(t, "root", "child", "root", "another-model")
		lease, err = f.router.acquire(t.Context(), familyTokenFromOptions(opts))
		if err != nil {
			t.Fatal(err)
		}
		if err := lease.validate(); err != nil {
			t.Fatal(err)
		}
		lease.release()
	}
	f.router.mu.Lock()
	changed := f.router.sequence != sequence || f.router.durable != sequence
	f.router.mu.Unlock()
	if changed {
		t.Fatal("steady request dirtied family state")
	}
	after, err := os.ReadFile(f.router.cfg.StatePath)
	if err != nil || string(after) != string(data) {
		t.Fatal("steady request rewrote state")
	}
}

func TestFamilyStateLoadValidatesDeepAncestryAndCycles(t *testing.T) {
	key, root := familyHash("codex-family", "root"), familyHash("codex-thread", "root")
	state := familyDiskState{Schema: 1, Sequence: 1, Families: map[string]*familyEntry{key: {Root: root, Generation: 1, LastSeen: time.Now().UTC()}},
		Members: map[string]familyMember{root: {Family: key, ParentKnown: true}}, Exhausted: map[string]familyExhaustion{}}
	parent := root
	for i := 0; i < 16384; i++ {
		next := familyHash("codex-thread", fmt.Sprintf("member-%05d", i))
		state.Members[next] = familyMember{Family: key, Parent: parent, ParentKnown: true}
		parent = next
	}
	path := filepath.Join(t.TempDir(), "families.json")
	data, _ := json.Marshal(state)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	r := newFamilyRouter(FamilyRoutingConfig{StatePath: path})
	if r.stateErr != nil {
		t.Fatal(r.stateErr)
	}
	t.Logf("validated %d members in %s", len(state.Members), time.Since(start))
	r.stop()
	// Turn the tail and its predecessor into a cycle without altering the root.
	previous := state.Members[parent].Parent
	state.Members[previous] = familyMember{Family: key, Parent: parent, ParentKnown: true}
	data, _ = json.Marshal(state)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	r = newFamilyRouter(FamilyRoutingConfig{StatePath: path})
	defer r.stop()
	if r.stateErr == nil {
		t.Fatal("cyclic on-disk ancestry accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(data) {
		t.Fatal("invalid state was not preserved")
	}
}

func TestFamilyStatePruningPreservesBusyFamilies(t *testing.T) {
	f := newFamilyFixture(t, 1, FamilyRoutingConfig{IdleRetention: 24 * time.Hour})
	_, busy := f.pick(t, "busy", "child", "busy", "model")
	lease, err := f.router.acquire(t.Context(), familyTokenFromOptions(busy))
	if err != nil {
		t.Fatal(err)
	}
	defer lease.release()
	f.pick(t, "idle", "idle", "", "model")
	f.router.mu.Lock()
	f.router.clock = func() time.Time { return f.now.Add(25 * time.Hour) }
	f.router.mu.Unlock()
	f.pick(t, "new", "new", "", "model")
	if status := f.router.status(); status.Families != 2 || status.Members != 3 {
		t.Fatalf("retention removed a busy family or retained idle state: %+v", status)
	}
	_, resumed := f.pick(t, "", "child", "", "model")
	if familyTokenFromOptions(resumed).family != familyTokenFromOptions(busy).family {
		t.Fatal("pruning lost busy descendant membership")
	}
}
