package codexmonitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

type memoryStore struct {
	state         control
	entries       map[string][]byte
	fail          bool
	writes, bytes int
}

func (s *memoryStore) load() (control, map[string]*Entry, error) {
	entries := map[string]*Entry{}
	for key, data := range s.entries {
		var e Entry
		if err := json.Unmarshal(data, &e); err != nil {
			return control{}, nil, err
		}
		entries[key] = &e
	}
	return s.state, entries, nil
}
func (s *memoryStore) saveControl(c control) error {
	if s.fail {
		return errors.New("injected disk failure")
	}
	s.state = c
	s.state.Starts = append([]time.Time{}, c.Starts...)
	s.writes++
	return nil
}
func (s *memoryStore) saveEntry(e *Entry) error {
	if s.fail {
		return errors.New("injected disk failure")
	}
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	s.entries[e.Key] = data
	s.writes++
	s.bytes += len(data)
	return nil
}
func (s *memoryStore) removeEntry(key string) error { delete(s.entries, key); return nil }
func (s *memoryStore) Close() error                 { return nil }

func rig(t *testing.T, count int) (*Coordinator, *memoryStore, []Identity, *time.Time) {
	t.Helper()
	s := &memoryStore{state: control{Schema: 1}, entries: map[string][]byte{}}
	c, err := New(s)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	c.clock = func() time.Time { return now }
	c.random = func(n int64) int64 { return n / 2 }
	ids := make([]Identity, count)
	for i := range ids {
		ids[i] = Identity{Key: fmt.Sprintf("%064x", i+1), AuthIndex: fmt.Sprint(i + 1), Enabled: true, Active: i < 2, Priority: count - i}
	}
	return c, s, ids, &now
}

func usageAt(now time.Time) *Usage {
	return &Usage{ObservedAt: now, Source: "provider_check", Plan: "pro", Windows: []Window{{Kind: "weekly", Label: "Weekly", UsedPercent: 28, DurationSeconds: 604800, ResetAt: now.Add(7 * 24 * time.Hour)}}}
}

func bankAt(now time.Time) *Bank {
	count := 0
	return &Bank{ObservedAt: now, AvailableCount: &count, Credits: []Grant{}, Complete: true}
}

func TestManualOnlyAndIndependentQueues(t *testing.T) {
	c, _, ids, now := rig(t, 3)
	calls := 0
	fetch := func(_ context.Context, _ Identity, kind string) Result {
		calls++
		if kind != UsageLane {
			t.Fatalf("usage refresh also fetched %s", kind)
		}
		return Result{Status: 200, Usage: usageAt(*now)}
	}
	for i := 0; i < 5; i++ {
		if _, err := c.Step(context.Background(), ids, Request{}, fetch); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(24 * time.Hour)
	}
	if calls != 0 {
		t.Fatal("manual-only dispatched")
	}
	snapshot, err := c.Step(context.Background(), ids, Request{Refresh: UsageLane}, fetch)
	if err != nil || calls != 1 || snapshot.Pending != 2 {
		t.Fatalf("first: %d %+v %v", calls, snapshot, err)
	}
	for i := 0; i < 100; i++ {
		if _, err := c.Step(context.Background(), ids, Request{Refresh: UsageLane}, fetch); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatal("repeated clicks dispatched duplicates")
	}
	for i := 0; i < 2; i++ {
		*now = now.Add(10 * time.Second)
		if _, err := c.Step(context.Background(), ids, Request{}, fetch); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 3 {
		t.Fatalf("queue count %d", calls)
	}
	for _, e := range c.entries {
		if !e.ResetSchedule.LastAttempt.IsZero() {
			t.Fatal("usage queue touched reset cadence")
		}
	}
}

func TestDailyAttemptSurvivesFailureAndRestart(t *testing.T) {
	c, store, ids, now := rig(t, 1)
	calls := 0
	fetch := func(context.Context, Identity, string) Result { calls++; return Result{Status: 503} }
	if _, err := c.Step(context.Background(), ids, Request{Refresh: ResetLane}, fetch); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal(calls)
	}
	restarted, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	restarted.clock, restarted.random = c.clock, c.random
	for i := 0; i < 24; i++ {
		*now = now.Add(time.Hour)
		if _, err := restarted.Step(context.Background(), ids, Request{Policy: Policy{ResetSeconds: 86400}}, fetch); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatal("automatic reset inventory repeated within 24 hours")
	}
	*now = now.Add(2 * time.Hour)
	if _, err := restarted.Step(context.Background(), ids, Request{Policy: Policy{ResetSeconds: 86400}}, fetch); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("next daily attempt missing: %d", calls)
	}
}

func TestWeeklyAndManualOverrideRespectCooldown(t *testing.T) {
	c, _, ids, now := rig(t, 1)
	start := *now
	calls := 0
	fetch := func(context.Context, Identity, string) Result {
		calls++
		return Result{Status: 429, RetryAt: start.Add(48 * time.Hour)}
	}
	_, err := c.Step(context.Background(), ids, Request{Policy: Policy{ResetSeconds: 604800}, Refresh: ResetLane}, fetch)
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(24 * time.Hour)
	_, err = c.Step(context.Background(), ids, Request{Policy: Policy{ResetSeconds: 604800}, Refresh: ResetLane}, fetch)
	if err != nil || calls != 1 {
		t.Fatalf("cooldown bypass: %d %v", calls, err)
	}
	*now = start.Add(3 * 24 * time.Hour)
	_, err = c.Step(context.Background(), ids, Request{Policy: Policy{ResetSeconds: 604800}}, fetch)
	if err != nil || calls != 1 {
		t.Fatal("weekly cadence bypassed")
	}
	_, err = c.Step(context.Background(), ids, Request{Refresh: ResetLane}, func(context.Context, Identity, string) Result {
		calls++
		return Result{Status: 200, Bank: bankAt(*now)}
	})
	if err != nil || calls != 2 {
		t.Fatalf("manual override unavailable: %d %v", calls, err)
	}
}

func TestSingleFlightConcurrentConsumers(t *testing.T) {
	c, _, ids, now := rig(t, 2)
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		_, err := c.Step(context.Background(), ids, Request{Refresh: UsageLane}, func(context.Context, Identity, string) Result {
			close(started)
			<-release
			return Result{Status: 200, Usage: usageAt(*now)}
		})
		if err != nil {
			t.Error(err)
		}
	}()
	<-started
	var clients sync.WaitGroup
	for i := 0; i < 20; i++ {
		clients.Add(1)
		go func() {
			defer clients.Done()
			snapshot, err := c.Step(context.Background(), ids, Request{Refresh: UsageLane}, func(context.Context, Identity, string) Result { t.Error("overlapping read"); return Result{} })
			if err != nil || !snapshot.InFlight {
				t.Errorf("concurrent snapshot: %v %v", snapshot.InFlight, err)
			}
		}()
	}
	clients.Wait()
	if err := c.Close(); err == nil {
		t.Fatal("released ownership during I/O")
	}
	close(release)
	<-done
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestFair128AccountBudgetAndNoStartupSweep(t *testing.T) {
	c, store, ids, now := rig(t, 128)
	seen := map[string]int{}
	starts := []time.Time{}
	fetch := func(_ context.Context, id Identity, kind string) Result {
		seen[id.Key]++
		starts = append(starts, *now)
		return Result{Status: 200, Bank: bankAt(*now)}
	}
	if _, err := c.Step(context.Background(), ids, Request{Policy: Policy{UsageSeconds: 1800, ResetSeconds: 86400}}, fetch); err != nil {
		t.Fatal(err)
	}
	if len(starts) != 0 {
		t.Fatal("startup sweep")
	}
	for i := 0; i < 128; i++ {
		req := Request{}
		if i == 0 {
			req.Refresh = ResetLane
		}
		if _, err := c.Step(context.Background(), ids, req, fetch); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(10 * time.Second)
	}
	if len(seen) != 128 {
		t.Fatalf("starved accounts: got %d", len(seen))
	}
	for _, count := range seen {
		if count != 1 {
			t.Fatal("duplicate account attempt")
		}
	}
	for i, start := range starts {
		if i > 0 && start.Sub(starts[i-1]) < 10*time.Second {
			t.Fatal("request gap")
		}
		if i >= 6 && start.Sub(starts[i-6]) < time.Minute {
			t.Fatal("rolling request budget")
		}
	}
	if store.bytes > 2<<20 || store.writes > 128*5 {
		t.Fatalf("write amplification: %d bytes, %d writes", store.bytes, store.writes)
	}
}

func TestPassiveFreshnessIdentityAndExpiry(t *testing.T) {
	c, _, ids, now := rig(t, 1)
	u := usageAt(*now)
	u.Source = "passive"
	ids[0].Passive = u
	snapshot, err := c.Snapshot(ids, Policy{UsageSeconds: 1800})
	if err != nil {
		t.Fatal(err)
	}
	observed := snapshot.Accounts[0].Usage.ObservedAt
	*now = now.Add(10 * time.Minute)
	calls := 0
	_, err = c.Step(context.Background(), ids, Request{Policy: Policy{UsageSeconds: 1800}}, func(context.Context, Identity, string) Result { calls++; return Result{} })
	if err != nil || calls != 0 {
		t.Fatal("fresh passive reading caused fallback")
	}
	snapshot, _ = c.Snapshot(ids, Policy{})
	if !snapshot.Accounts[0].Usage.ObservedAt.Equal(observed) {
		t.Fatal("cache manufactured observation time")
	}
	ids[0].Key = fmt.Sprintf("%064x", 999)
	ids[0].Passive = nil
	snapshot, err = c.Snapshot(ids, Policy{})
	if err != nil || snapshot.Accounts[0].Usage != nil || len(c.entries) != 1 {
		t.Fatal("replacement inherited old balance")
	}
	ids[0].Passive = usageAt(*now)
	ids[0].Passive.Windows[0].ResetAt = now.Add(time.Minute)
	_, _ = c.Snapshot(ids, Policy{})
	*now = now.Add(2 * time.Minute)
	snapshot, _ = c.Snapshot(ids, Policy{})
	if snapshot.Accounts[0].Usage != nil {
		t.Fatal("expired window stayed authoritative")
	}
}

func TestPersistenceFailureBeforeDispatchAndCrashClaim(t *testing.T) {
	c, store, ids, now := rig(t, 1)
	store.fail = true
	calls := 0
	_, err := c.Step(context.Background(), ids, Request{Refresh: ResetLane}, func(context.Context, Identity, string) Result { calls++; return Result{} })
	if err == nil || calls != 0 {
		t.Fatal("sent without durable attempt")
	}
	store.fail = false
	_, err = c.Step(context.Background(), ids, Request{Refresh: ResetLane}, func(context.Context, Identity, string) Result {
		// This is the durable state an abruptly terminated process would leave.
		restarted, errNew := New(store)
		if errNew != nil {
			t.Fatal(errNew)
		}
		restarted.clock, restarted.random = c.clock, c.random
		*now = now.Add(2 * time.Minute)
		_, errStep := restarted.Step(context.Background(), ids, Request{Policy: Policy{ResetSeconds: 86400}}, func(context.Context, Identity, string) Result { t.Error("crash replayed attempt"); return Result{} })
		if errStep != nil {
			t.Fatal(errStep)
		}
		return Result{Status: 503}
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestBlockedMissingDuplicateAndClockReversal(t *testing.T) {
	c, _, ids, now := rig(t, 1)
	ids[0].Blocked = true
	fetch := func(context.Context, Identity, string) Result { t.Error("blocked dispatch"); return Result{} }
	_, err := c.Step(context.Background(), ids, Request{Refresh: UsageLane}, fetch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Snapshot(append(ids, ids[0]), Policy{}); err == nil {
		t.Fatal("accepted duplicate identity")
	}
	ids[0].Blocked = false
	ids[0].Passive = usageAt(*now)
	_, _ = c.Snapshot(ids, Policy{})
	*now = now.Add(-time.Hour)
	snapshot, err := c.Snapshot(ids, Policy{})
	if err != nil || snapshot.Accounts[0].Usage != nil {
		t.Fatal("future observation exposed as current")
	}
}

func TestErrorsAndResetExpiryKeepUsageIndependent(t *testing.T) {
	for _, status := range []int{401, 403, 429, 500, 200} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			c, _, ids, now := rig(t, 1)
			ids[0].Passive = usageAt(*now)
			snapshot, err := c.Step(context.Background(), ids, Request{Refresh: ResetLane}, func(context.Context, Identity, string) Result { return Result{Status: status} })
			if err != nil || snapshot.Accounts[0].Usage == nil || snapshot.Accounts[0].ResetSchedule.Error == "" {
				t.Fatalf("lost independent usage: %v", err)
			}
		})
	}
	now := time.Now().UTC()
	expiry := now.Add(time.Minute)
	count := 1
	bank := &Bank{ObservedAt: now, AvailableCount: &count, Complete: true, Credits: []Grant{{ID: "fake-grant", Status: "available", ExpiresAt: &expiry}}}
	after := cloneBank(bank, now.Add(2*time.Minute))
	if after.AvailableCount != nil || after.Complete || bank.AvailableCount == nil {
		t.Fatal("grant expiry or copy isolation")
	}
}

func TestAccountCooldownBlocksOtherLaneAndSurvivesRestart(t *testing.T) {
	for _, status := range []int{401, 403, 429, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			c, store, ids, now := rig(t, 1)
			floor := now.Add(2 * time.Hour)
			_, err := c.Step(context.Background(), ids, Request{Refresh: UsageLane}, func(context.Context, Identity, string) Result {
				return Result{Status: status, RetryAt: floor}
			})
			if err != nil {
				t.Fatal(err)
			}
			restarted, err := New(store)
			if err != nil {
				t.Fatal(err)
			}
			restarted.clock, restarted.random = c.clock, c.random
			*now = now.Add(time.Hour)
			snapshot, err := restarted.Step(context.Background(), ids, Request{Refresh: ResetLane}, func(context.Context, Identity, string) Result {
				t.Fatal("cross-lane cooldown bypass")
				return Result{}
			})
			if err != nil || !snapshot.Accounts[0].ResetSchedule.RetryAt.Equal(floor) || snapshot.Accounts[0].ResetSchedule.Error != "" {
				t.Fatalf("independent errors/shared deadline: %+v %v", snapshot, err)
			}
		})
	}
}

func TestLongWholePoolQueuesCoalesceAndFinishAtMaximumGap(t *testing.T) {
	c, _, ids, now := rig(t, MaxAccounts)
	seen := map[string]int{}
	fetch := func(_ context.Context, id Identity, kind string) Result {
		seen[id.Key+kind]++
		return Result{Status: 200, Usage: usageAt(*now), Bank: bankAt(*now)}
	}
	policy := Policy{GapSeconds: 60}
	for _, kind := range []string{UsageLane, ResetLane} {
		if _, err := c.Step(context.Background(), ids, Request{Policy: policy, Refresh: kind}, fetch); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i < 2*MaxAccounts; i++ {
		*now = now.Add(time.Minute)
		// Repeated whole-pool clicks while that lane is still pending must not
		// put previously served accounts at the back of the same batch.
		req := Request{Policy: policy}
		if i < 100 {
			req.Refresh = ResetLane
		}
		if _, err := c.Step(context.Background(), ids, req, fetch); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := c.Snapshot(ids, policy)
	if err != nil || snapshot.Pending != 0 || len(seen) != 2*MaxAccounts {
		t.Fatalf("queue incomplete: %d %d %v", snapshot.Pending, len(seen), err)
	}
	for _, n := range seen {
		if n != 1 {
			t.Fatal("repeated batch requeued an early account")
		}
	}
}

func TestPendingExpiryAndFetchPanicReleaseOwnership(t *testing.T) {
	c, _, ids, now := rig(t, 2)
	snapshot, err := c.Step(context.Background(), ids, Request{Refresh: ResetLane}, func(context.Context, Identity, string) Result { panic("synthetic adapter fault") })
	if err != nil || snapshot.InFlight || snapshot.Accounts[0].ResetSchedule.Error != "provider_unavailable" {
		t.Fatalf("panic recovery: %+v %v", snapshot, err)
	}
	*now = now.Add(MaxPendingAge + time.Minute)
	snapshot, err = c.Snapshot(ids, Policy{})
	if err != nil || snapshot.Pending != 0 || snapshot.Accounts[1].ResetSchedule.Error != "refresh_expired" {
		t.Fatalf("queue expiry: %+v %v", snapshot, err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNewerSuccessSupersedesOnlyOldOAuthDemandWithoutRelaxingFloors(t *testing.T) {
	for _, status := range []int{401, 403, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			c, store, ids, now := rig(t, 1)
			floor := now.Add(2 * time.Hour)
			failed, err := c.Step(context.Background(), ids, Request{Refresh: ResetLane}, func(context.Context, Identity, string) Result {
				return Result{Status: status, RetryAt: floor}
			})
			if err != nil {
				t.Fatal(err)
			}
			before := failed.Accounts[0].ResetSchedule
			// An old cached response is not recovery evidence.
			ids[0].Passive = usageAt(now.Add(-time.Second))
			s, err := c.Snapshot(ids, Policy{})
			if err != nil || s.Accounts[0].ResetSchedule.Error != before.Error {
				t.Fatal("old observation changed credential status", err)
			}
			*now = now.Add(time.Minute)
			ids[0].Passive = usageAt(*now)
			s, err = c.Snapshot(ids, Policy{})
			want := before.Error
			if status == 401 {
				want = "observation_needed"
			}
			after := s.Accounts[0].ResetSchedule
			if err != nil || after.Error != want || !after.ErrorAt.Equal(before.ErrorAt) ||
				!after.LastAttempt.Equal(before.LastAttempt) || !after.DueAt.Equal(before.DueAt) ||
				!after.RetryAt.Equal(floor) || s.Accounts[0].Bank != nil {
				t.Fatal("recovery changed inventory or request floors", err)
			}
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := New(store)
			if err != nil {
				t.Fatal(err)
			}
			reopened.clock, reopened.random = c.clock, c.random
			s, err = reopened.Step(context.Background(), ids, Request{Refresh: ResetLane}, func(context.Context, Identity, string) Result {
				t.Fatal("recovery bypassed cooldown")
				return Result{}
			})
			if err != nil || s.Accounts[0].ResetSchedule.Error != want {
				t.Fatal("recovery did not survive restart", err)
			}
		})
	}
}

func TestDelayedSuccessCannotClearNewerUsageRejection(t *testing.T) {
	c, _, ids, now := rig(t, 1)
	_, err := c.Step(context.Background(), ids, Request{Refresh: UsageLane}, func(context.Context, Identity, string) Result {
		return Result{Status: 401}
	})
	if err != nil {
		t.Fatal(err)
	}
	ids[0].Passive = usageAt(now.Add(-time.Second))
	s, err := c.Snapshot(ids, Policy{})
	if err != nil || s.Accounts[0].UsageSchedule.Error != "oauth_rejected" {
		t.Fatal("old passive frame erased newer rejection", err)
	}
	err = c.ObserveRead(ids, ids[0].Key, UsageLane, Result{Status: 200, Usage: usageAt(now.Add(-time.Second))})
	s, _ = c.Snapshot(ids, Policy{})
	if err != nil || s.Accounts[0].UsageSchedule.Error != "oauth_rejected" {
		t.Fatal("delayed explicit read erased newer rejection", err)
	}
}
