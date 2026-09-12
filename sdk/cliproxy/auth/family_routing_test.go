package auth

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type familyFixture struct {
	mu          sync.Mutex
	router      *familyRouter
	auths       []*Auth
	quota       map[string]FamilyQuotaObservation
	blocked     map[string]bool
	unsupported map[string]bool
	now         time.Time
}

func newFamilyFixture(t *testing.T, accounts int, cfg FamilyRoutingConfig) *familyFixture {
	t.Helper()
	if cfg.StatePath == "" {
		cfg.StatePath = filepath.Join(t.TempDir(), "families.json")
	}
	f := &familyFixture{router: newFamilyRouter(cfg), quota: map[string]FamilyQuotaObservation{},
		blocked: map[string]bool{}, unsupported: map[string]bool{}, now: time.Now().UTC()}
	for i := 0; i < accounts; i++ {
		a := &Auth{ID: fmt.Sprintf("family-auth-%02d", i), Index: fmt.Sprintf("index-%02d", i), Provider: "codex",
			Status: StatusActive, RegistrationEpoch: 1, indexAssigned: true,
			Attributes: map[string]string{"priority": fmt.Sprint((accounts - i) * 100)}}
		a.codexAccountIdentity = codexAccountIdentity(a)
		f.auths = append(f.auths, a)
		f.quota[a.ID] = FamilyQuotaObservation{Available: true, Identity: a.CodexAccountIdentity(), RegistrationEpoch: 1,
			ObservedAt: f.now, WeeklyKnown: true, WeeklyUsedPercent: 10, WeeklyResetAt: f.now.Add(7 * 24 * time.Hour)}
	}
	f.attach()
	if f.router.stateErr != nil {
		t.Fatal(f.router.stateErr)
	}
	t.Cleanup(func() { f.router.stop() })
	return f
}

func (f *familyFixture) attach() {
	f.router.attach(func(a *Auth) FamilyQuotaObservation {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.quota[a.ID]
	}, func(index, identity, model string) (*Auth, bool, bool) {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, a := range f.auths {
			if a.Index == index && a.CodexAccountIdentity() == identity {
				return a.Clone(), !f.unsupported[a.ID+":"+model], f.blocked[a.ID]
			}
		}
		return nil, false, false
	})
}

func familyOptions(root, thread, parent string) cliproxyexecutor.Options {
	headers := make(http.Header)
	if root != "" {
		headers.Set("Session-Id", root)
	}
	if thread != "" {
		headers.Set("Thread-Id", thread)
	}
	if parent != "" {
		headers.Set("X-Codex-Parent-Thread-Id", parent)
	}
	return cliproxyexecutor.Options{Headers: headers, Metadata: map[string]any{}}
}

func (f *familyFixture) pick(t *testing.T, root, thread, parent, model string) (*Auth, cliproxyexecutor.Options) {
	t.Helper()
	opts := familyOptions(root, thread, parent)
	a, err := f.router.pick(t.Context(), model, opts, f.auths)
	if err != nil {
		t.Fatal(err)
	}
	return a, opts
}

func (f *familyFixture) exhaust(index int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.auths[index]
	q := f.quota[a.ID]
	q.WeeklyUsedPercent = 100
	f.quota[a.ID] = q
}

func TestFamilyRoutingBalancesAllEligiblePriorities(t *testing.T) {
	f := newFamilyFixture(t, 10, FamilyRoutingConfig{})
	for i := 0; i < 6; i++ {
		f.exhaust(i)
	}
	counts := map[string]int{}
	for i := 0; i < 20; i++ {
		root := fmt.Sprintf("root-%02d", i)
		a, opts := f.pick(t, root, root, "", "model-a")
		lease, err := f.router.acquire(t.Context(), familyTokenFromOptions(opts))
		if err != nil {
			t.Fatal(err)
		}
		if err := lease.validate(); err != nil {
			t.Fatal(err)
		}
		lease.release()
		counts[a.ID]++
		child, _ := f.pick(t, root, root+"-child", root, "model-b")
		grandchild, _ := f.pick(t, root, root+"-grandchild", root+"-child", "model-c")
		if child.ID != a.ID || grandchild.ID != a.ID {
			t.Fatal("family split across models")
		}
	}
	for i, a := range f.auths {
		want := 5
		if i < 6 {
			want = 0
		}
		if counts[a.ID] != want {
			t.Fatalf("account %d got %d root families, want %d", i, counts[a.ID], want)
		}
	}
	if status := f.router.status(); status.WeeklyExcludedAccounts != 6 || status.Families != 20 || status.Members != 60 {
		t.Fatalf("unexpected bounded state: %+v", status)
	}
}

func TestFamilyRoutingAtomicColdDescendantsAndRestart(t *testing.T) {
	f := newFamilyFixture(t, 4, FamilyRoutingConfig{})
	start := make(chan struct{})
	results := make(chan *familySelection, 60)
	errors := make(chan error, 60)
	var wg sync.WaitGroup
	for i := 0; i < 60; i++ {
		wg.Go(func() {
			<-start
			thread, parent := "root", ""
			switch i % 3 {
			case 1:
				thread, parent = "child", "root"
			case 2:
				thread, parent = "grandchild", "child"
			}
			opts := familyOptions("root", thread, parent)
			_, err := f.router.pick(t.Context(), fmt.Sprintf("model-%d", i), opts, f.auths)
			if err != nil {
				errors <- err
				return
			}
			results <- familyTokenFromOptions(opts)
		})
	}
	close(start)
	wg.Wait()
	close(errors)
	for err := range errors {
		t.Fatal(err)
	}
	close(results)
	var first *familySelection
	for token := range results {
		if first == nil {
			first = token
		}
		if token.identity != first.identity || token.generation != first.generation {
			t.Fatal("cold allocation was not atomic")
		}
	}
	lease, err := f.router.acquire(t.Context(), first)
	if err != nil {
		t.Fatal(err)
	}
	lease.release()
	path := f.router.cfg.StatePath
	f.router.stop()
	f.router = newFamilyRouter(FamilyRoutingConfig{StatePath: path})
	f.attach()
	a, opts := f.pick(t, "", "grandchild", "", "another-model")
	if a.CodexAccountIdentity() != first.identity || familyTokenFromOptions(opts).generation != first.generation {
		t.Fatal("cold resume lost assignment")
	}
	if status := f.router.status(); status.Families != 1 || status.Members != 3 {
		t.Fatalf("lost membership: %+v", status)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"grandchild", "family-auth", `"root":"root"`} {
		if strings.Contains(string(data), private) {
			t.Fatalf("state contains raw identifier %q", private)
		}
	}
}

func TestFamilyRoutingResetGenerationAndPinnedReplay(t *testing.T) {
	f := newFamilyFixture(t, 1, FamilyRoutingConfig{})
	a, oldOpts := f.pick(t, "root", "child", "root", "model")
	old := familyTokenFromOptions(oldOpts)
	if f.router.resetAssignments() != 1 || f.router.resetAssignments() != 0 {
		t.Fatal("reset not idempotent")
	}
	pinned := familyOptions("root", "child", "root")
	pinned.Metadata[cliproxyexecutor.PinnedAuthMetadataKey] = a.ID
	if _, err := f.router.pick(t.Context(), "model", pinned, f.auths); !isFamilyReselect(err) {
		t.Fatalf("pin rebound without full replay: %v", err)
	}
	_, freshOpts := f.pick(t, "root", "grandchild", "child", "model")
	fresh := familyTokenFromOptions(freshOpts)
	if fresh.generation <= old.generation {
		t.Fatal("reset did not advance generation")
	}
	f.router.onResult(old, Result{AuthID: a.ID, Error: &Error{HTTPStatus: 503}})
	f.router.mu.Lock()
	_, err := f.router.validateSelectionLocked(fresh)
	f.router.mu.Unlock()
	if err != nil {
		t.Fatalf("stale completion cleared new assignment: %v", err)
	}
	if f.router.status().Members != 3 {
		t.Fatal("reset discarded transitive membership")
	}
	for _, thread := range []string{"root", "child", "grandchild"} {
		resumed, _ := f.pick(t, "", thread, "", "model")
		if resumed.ID != a.ID {
			t.Fatalf("reset lost %s's family membership", thread)
		}
	}
}

func TestFamilyRoutingModelConflictPreservesOwnerAndExhaustionRebinds(t *testing.T) {
	f := newFamilyFixture(t, 2, FamilyRoutingConfig{})
	a, _ := f.pick(t, "root", "root", "", "model-a")
	f.mu.Lock()
	f.unsupported[a.ID+":model-b"] = true
	f.mu.Unlock()
	if _, err := f.router.pick(t.Context(), "model-b", familyOptions("root", "root", ""), f.auths); statusCodeFromError(err) != 409 {
		t.Fatalf("incompatible healthy owner split: %v", err)
	}
	again, _ := f.pick(t, "root", "root", "", "model-a")
	if again.ID != a.ID {
		t.Fatal("model conflict changed binding")
	}
	f.exhaust(0)
	b, _ := f.pick(t, "root", "child", "root", "model-a")
	if b.ID == a.ID {
		t.Fatal("confirmed exhausted owner retained")
	}
	again, _ = f.pick(t, "root", "root", "", "model-a")
	if again.ID != b.ID {
		t.Fatal("whole family did not move")
	}
}

func TestFamilyRoutingQuotaUnknownAndEpochIsolation(t *testing.T) {
	f := newFamilyFixture(t, 1, FamilyRoutingConfig{})
	f.mu.Lock()
	a := f.auths[0]
	q := f.quota[a.ID]
	q.WeeklyKnown = false
	f.quota[a.ID] = q
	f.mu.Unlock()
	_, opts := f.pick(t, "root", "root", "", "model")
	old := familyTokenFromOptions(opts)
	f.mu.Lock()
	a.RegistrationEpoch++
	q.RegistrationEpoch = a.RegistrationEpoch
	f.quota[a.ID] = q
	f.mu.Unlock()
	_, freshOpts := f.pick(t, "root", "root", "", "model")
	fresh := familyTokenFromOptions(freshOpts)
	f.router.mu.Lock()
	_, errOld := f.router.validateSelectionLocked(old)
	_, errFresh := f.router.validateSelectionLocked(fresh)
	f.router.mu.Unlock()
	if !isFamilyReselect(errOld) || errFresh != nil {
		t.Fatalf("old registration harmed new one: old %v fresh %v", errOld, errFresh)
	}
	f.mu.Lock()
	q.RegistrationEpoch--
	f.quota[a.ID] = q
	f.mu.Unlock()
	if _, err := f.router.pick(t.Context(), "model", familyOptions("other", "other", ""), f.auths); statusCodeFromError(err) != 503 {
		t.Fatalf("accepted stale quota epoch: %v", err)
	}
}

func TestFamilyRoutingExhaustionSurvivesPartialPositiveAndRestart(t *testing.T) {
	f := newFamilyFixture(t, 2, FamilyRoutingConfig{})
	f.exhaust(0)
	_, opts := f.pick(t, "root", "root", "", "model")
	lease, err := f.router.acquire(t.Context(), familyTokenFromOptions(opts))
	if err != nil {
		t.Fatal(err)
	}
	lease.release()
	f.mu.Lock()
	q := f.quota[f.auths[0].ID]
	q.WeeklyUsedPercent = 0
	q.ObservedAt = q.ObservedAt.Add(time.Second)
	f.quota[f.auths[0].ID] = q
	f.mu.Unlock()
	path := f.router.cfg.StatePath
	f.router.stop()
	f.router = newFamilyRouter(FamilyRoutingConfig{StatePath: path})
	f.attach()
	for i := 0; i < 3; i++ {
		a, _ := f.pick(t, fmt.Sprint(i), fmt.Sprint(i), "", "model")
		if a.ID == f.auths[0].ID {
			t.Fatal("restart/positive reading erased confirmed exhaustion")
		}
	}
	// Expire evidence using the router's deterministic clock; never sleep.
	f.router.mu.Lock()
	f.router.clock = func() time.Time { return q.WeeklyResetAt.Add(time.Second) }
	f.router.mu.Unlock()
	a, _ := f.pick(t, "post-reset", "post-reset", "", "model")
	if a.ID != f.auths[0].ID {
		t.Fatal("applicable reset did not release exhaustion")
	}
}

func TestFamilyRoutingStateFailuresAreClosedAndPreserved(t *testing.T) {
	t.Run("exclusive lifetime lock", func(t *testing.T) {
		f := newFamilyFixture(t, 1, FamilyRoutingConfig{})
		second := newFamilyRouter(f.router.cfg)
		defer second.stop()
		if second.stateErr == nil {
			t.Fatal("second writer acquired state")
		}
	})
	t.Run("corrupt source", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state.json")
		data := []byte(`{"schema":1,"schema":2}`)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		r := newFamilyRouter(FamilyRoutingConfig{StatePath: path})
		defer r.stop()
		if r.stateErr == nil {
			t.Fatal("corrupt state accepted")
		}
		after, _ := os.ReadFile(path)
		if string(after) != string(data) {
			t.Fatal("corrupt state was changed")
		}
	})
	t.Run("write fails before provider", func(t *testing.T) {
		f := newFamilyFixture(t, 1, FamilyRoutingConfig{})
		// A directory at the exact target makes atomic replacement fail on all platforms.
		if err := os.Mkdir(f.router.cfg.StatePath, 0700); err != nil {
			t.Fatal(err)
		}
		_, opts := f.pick(t, "root", "root", "", "model")
		if _, err := f.router.acquire(t.Context(), familyTokenFromOptions(opts)); statusCodeFromError(err) != 503 {
			t.Fatalf("write failure admitted request: %v", err)
		}
	})
}

func TestFamilyRoutingBoundedCapacityAndNoEligible(t *testing.T) {
	f := newFamilyFixture(t, 1, FamilyRoutingConfig{MaxFamilies: 1, MaxMembers: 2})
	f.pick(t, "root", "root", "", "model")
	if _, err := f.router.pick(context.Background(), "model", familyOptions("other", "other", ""), f.auths); statusCodeFromError(err) != 503 {
		t.Fatalf("family cap: %v", err)
	}
	f.exhaust(0)
	if _, err := f.router.pick(context.Background(), "model", familyOptions("root", "root", ""), f.auths); statusCodeFromError(err) != 503 {
		t.Fatalf("exhausted pool: %v", err)
	}
	if len(f.router.families) != 1 {
		t.Fatal("cap evicted existing family")
	}
}
