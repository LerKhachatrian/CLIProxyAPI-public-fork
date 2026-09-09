package codexmonitor

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Simulate fsync latency with the existing controllable clock; no sleep or
// machine load is needed to reproduce admission-versus-dispatch skew.
type delayedControlStore struct {
	*memoryStore
	now    *time.Time
	delay  time.Duration
	calls  int
	failAt int
}

func (s *delayedControlStore) saveControl(state control) error {
	s.calls++
	if s.calls == s.failAt {
		return errors.New("synthetic corrected budget failure")
	}
	err := s.memoryStore.saveControl(state)
	*s.now = s.now.Add(s.delay)
	s.delay = 0
	return err
}

func TestDispatchPacingPreservesChosenAutomaticResetJitter(t *testing.T) {
	for _, maximum := range []bool{false, true} {
		c, store, ids, now := rig(t, 3)
		c.random = func(n int64) int64 {
			if maximum {
				return n - 1
			}
			return 0
		}
		makeResetsDue(t, c, ids, *now)
		c.store = &delayedControlStore{memoryStore: store, now: now, delay: time.Second}
		var started time.Time
		calls := 0
		fetch := func(_ context.Context, _ Identity, kind string) Result {
			calls++
			if calls == 1 {
				started = *now
			}
			if kind == UsageLane {
				return Result{Status: 200, Usage: usageAt(*now)}
			}
			return Result{Status: 200, Bank: bankAt(*now)}
		}
		policy := Policy{ResetSeconds: 86400}
		if _, err := c.Step(context.Background(), ids, Request{Policy: policy}, fetch); err != nil {
			t.Fatal(err)
		}
		interval := time.Minute
		if maximum {
			interval = 2*time.Minute - time.Nanosecond
		}
		deadline := started.Add(interval)
		if !store.state.AutomaticReset.LastStart.Equal(started) || !store.state.AutomaticReset.NextStart.Equal(deadline) {
			t.Fatal("dispatch correction redrew or shortened reset jitter")
		}
		// A completed check has a corrected durable start, not an interrupted
		// claim. Ordinary restart retains that deadline without a recovery fence.
		restarted, err := New(store)
		if err != nil {
			t.Fatal(err)
		}
		restarted.clock = c.clock
		*now = started.Add(10 * time.Second)
		if _, err := restarted.Step(context.Background(), ids, Request{Policy: policy, Refresh: UsageLane, AuthIndex: ids[1].AuthIndex}, fetch); err != nil || calls != 2 {
			t.Fatal("automatic reset floor delayed independent manual usage", err)
		}
		*now = deadline.Add(-time.Nanosecond)
		if _, err := restarted.Step(context.Background(), ids, Request{Policy: policy}, fetch); err != nil || calls != 2 {
			t.Fatal("reset started before the corrected floor", err)
		}
		*now = deadline
		if _, err := restarted.Step(context.Background(), ids, Request{Policy: policy}, fetch); err != nil || calls != 3 {
			t.Fatal("reset did not resume at its original chosen interval", err)
		}
	}
}

func TestDispatchPacingInterruptedCorrectionIsConservativeOnRecovery(t *testing.T) {
	for _, reset := range []bool{false, true} {
		c, store, ids, now := rig(t, 3)
		delayed := &delayedControlStore{memoryStore: store, now: now, delay: time.Second, failAt: 2}
		c.store = delayed
		request := Request{Refresh: UsageLane, AuthIndex: ids[0].AuthIndex}
		if reset {
			makeResetsDue(t, c, ids, *now)
			request = Request{Policy: Policy{ResetSeconds: 86400}}
		}
		calls := 0
		fetch := func(_ context.Context, _ Identity, kind string) Result {
			calls++
			if kind == ResetLane {
				return Result{Status: 200, Bank: bankAt(*now)}
			}
			return Result{Status: 200, Usage: usageAt(*now)}
		}
		snapshot, err := c.Step(context.Background(), ids, request, fetch)
		if err != nil || calls != 1 || snapshot.Error != "persistence_unavailable" {
			t.Fatalf("correction failure not surfaced: calls=%d snapshot=%+v err=%v", calls, snapshot, err)
		}
		restarted, err := New(store)
		if err != nil {
			t.Fatal(err)
		}
		restarted.clock = c.clock
		ids = ids[1:] // Fence before retiring the interrupted account.
		recoveredAt := *now
		if _, err := restarted.Snapshot(ids, Policy{}); err != nil {
			t.Fatal(err)
		}
		if !store.state.NextStart.Equal(recoveredAt.Add(time.Minute)) {
			t.Fatal("unknown dispatch lost maximum shared-gap recovery fence")
		}
		if reset && !store.state.AutomaticReset.NextStart.Equal(recoveredAt.Add(2*time.Minute)) {
			t.Fatal("unknown reset dispatch lost maximum automatic-reset fence")
		}
		for i := 0; i < 3; i++ {
			*now = recoveredAt.Add(time.Duration(i+1) * time.Second)
			if _, err := restarted.Step(context.Background(), ids, Request{Refresh: UsageLane}, fetch); err != nil || calls != 1 {
				t.Fatal("recovery allowed early reads", err)
			}
			if !store.state.NextStart.Equal(recoveredAt.Add(time.Minute)) {
				t.Fatal("views slid recovery fence")
			}
		}
		*now = recoveredAt.Add(time.Minute)
		if _, err := restarted.Step(context.Background(), ids, Request{}, fetch); err != nil || calls != 2 {
			t.Fatal("recovery delayed usage beyond the bounded shared fence", err)
		}
	}
}

func TestDispatchPacingRecoveryFailureDoesNotSendOrDropClaim(t *testing.T) {
	c, store, ids, now := rig(t, 2)
	if _, err := c.Snapshot(ids, Policy{}); err != nil {
		t.Fatal(err)
	}
	entry := c.entries[ids[0].Key]
	entry.UsageSchedule.Error = "interrupted_check"
	entry.UsageSchedule.LastAttempt = *now
	if err := store.saveEntry(entry); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	restarted.clock = c.clock
	store.fail = true
	if _, err := restarted.Step(context.Background(), ids[1:], Request{Refresh: UsageLane}, func(context.Context, Identity, string) Result {
		t.Fatal("provider reached during failed budget recovery")
		return Result{}
	}); err == nil {
		t.Fatal("failed recovery was not reported")
	}
	if _, exists := store.entries[entry.Key]; !exists || !restarted.recoverRead {
		t.Fatal("failed recovery erased its pending durable claim")
	}
}

func TestDispatchPacingIncludesPreflightPersistenceLatency(t *testing.T) {
	c, store, ids, now := rig(t, 2)
	initial := *now
	c.store = &delayedControlStore{memoryStore: store, now: now, delay: time.Second}
	var starts []time.Time
	fetch := func(_ context.Context, _ Identity, _ string) Result {
		starts = append(starts, *now)
		return Result{Status: 200, Usage: usageAt(*now)}
	}
	if _, err := c.Step(context.Background(), ids, Request{Refresh: UsageLane}, fetch); err != nil {
		t.Fatal(err)
	}
	*now = initial.Add(10 * time.Second)
	if _, err := c.Step(context.Background(), ids, Request{}, fetch); err != nil {
		t.Fatal(err)
	}
	if len(starts) != 1 {
		t.Fatalf("persistence shortened actual dispatch spacing to %s", starts[1].Sub(starts[0]))
	}
	*now = starts[0].Add(10 * time.Second)
	if _, err := c.Step(context.Background(), ids, Request{}, fetch); err != nil || len(starts) != 2 {
		t.Fatalf("dispatch missing at unchanged ten-second floor: starts=%v err=%v", starts, err)
	}
}
