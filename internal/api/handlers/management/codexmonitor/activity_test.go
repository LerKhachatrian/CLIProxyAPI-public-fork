package codexmonitor

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestActivityClassIntervalsAndStableJitter(t *testing.T) {
	c, _, ids, now := rig(t, 3)
	ids[0].InUse = true
	ids[2].Active = false
	policy := Policy{UsageSeconds: 1800}
	for _, endpoint := range []string{"minimum", "midpoint", "maximum"} {
		c.random = func(n int64) int64 {
			if endpoint == "minimum" {
				return 0
			}
			if endpoint == "maximum" {
				return n - 1
			}
			return n / 2
		}
		for i, bounds := range [][2]time.Duration{{5 * time.Minute, 10 * time.Minute}, {20 * time.Minute, 40 * time.Minute}, {20 * time.Hour, 28 * time.Hour}} {
			delay := c.usageDelay(ids[i], policy, *now)
			if delay < bounds[0] || delay >= bounds[1] {
				t.Fatalf("%s %s delay %s outside %v", endpoint, ids[i].activityClass(), delay, bounds)
			}
		}
	}
	seen := map[time.Duration]bool{}
	for i := 0; i < 128; i++ {
		key := fmt.Sprintf("%064x", i+1)
		anchor := now.Add(time.Duration(i) * time.Second)
		delay := activeUsageDelay(key, anchor)
		if delay < 5*time.Minute || delay >= 10*time.Minute || delay != activeUsageDelay(key, anchor.In(time.FixedZone("offset", 3600))) {
			t.Fatal("active jitter range or timestamp normalization")
		}
		seen[delay] = true
	}
	if len(seen) < 120 {
		t.Fatal("active accounts were synchronized rather than spread")
	}
	if got := c.usageDelay(ids[1], Policy{UsageSeconds: 3600}, *now); got < 40*time.Minute || got >= 80*time.Minute {
		t.Fatal("likely-next custom target was changed")
	}
	if c.usageDelay(ids[0], Policy{UsageSeconds: 7200}, *now) != c.usageDelay(ids[0], policy, *now) {
		t.Fatal("the active tier inherited the likely-next interval")
	}
}

func TestActivityPromotionUsesRealCaptureAndDoesNotRedrawOnViewsOrRestart(t *testing.T) {
	c, store, ids, now := rig(t, 1)
	ids[0].Active = false
	ids[0].Passive = usageAt(*now)
	policy := Policy{UsageSeconds: 1800}
	if _, err := c.Snapshot(ids, policy); err != nil {
		t.Fatal(err)
	}
	initial := c.entries[ids[0].Key].UsageSchedule.DueAt
	*now = now.Add(time.Minute)
	ids[0].Active, ids[0].InUse = true, true
	snapshot, err := c.Snapshot(ids, policy)
	if err != nil {
		t.Fatal(err)
	}
	want := ids[0].Passive.ObservedAt.Add(activeUsageDelay(ids[0].Key, ids[0].Passive.ObservedAt))
	if got := snapshot.Accounts[0]; !got.UsageSchedule.DueAt.Equal(want) || !want.Before(initial) || got.Activity != "active" {
		t.Fatal("promotion did not use actual capture time")
	}
	if err := c.flush(true, *now); err != nil {
		t.Fatal(err)
	}
	writes := store.writes
	for i := 0; i < 50; i++ {
		*now = now.Add(30 * time.Second)
		copy, err := New(store)
		if err != nil {
			t.Fatal(err)
		}
		copy.clock, copy.random = c.clock, func(int64) int64 { t.Fatal("active deadline redrew random state"); return 0 }
		snapshot, err := copy.Snapshot(ids, policy)
		if err != nil || !snapshot.Accounts[0].UsageSchedule.DueAt.Equal(want) || !snapshot.Accounts[0].Usage.ObservedAt.Equal(ids[0].Passive.ObservedAt) {
			t.Fatal("view/restart rejuvenated capture or changed deadline", err)
		}
		if err := copy.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if store.writes != writes {
		t.Fatal("unchanged hot views/restarts rewrote the cache")
	}
}

func TestNewActiveIdentityKeepsOneBoundedDeadlineWithoutObservation(t *testing.T) {
	c, store, ids, now := rig(t, 1)
	ids[0].InUse = true
	start := *now
	snapshot, err := c.Snapshot(ids, Policy{})
	if err != nil {
		t.Fatal(err)
	}
	due := snapshot.Accounts[0].UsageSchedule.DueAt
	if delay := due.Sub(start); delay < 5*time.Minute || delay >= 10*time.Minute {
		t.Fatal("new active identity missed bounded initial spread")
	}
	for i := 0; i < 30; i++ {
		*now = now.Add(time.Minute)
		copy, err := New(store)
		if err != nil {
			t.Fatal(err)
		}
		copy.clock = c.clock
		snapshot, err = copy.Snapshot(ids, Policy{})
		if err != nil || !snapshot.Accounts[0].UsageSchedule.DueAt.Equal(due) {
			t.Fatal("missing observation allowed sliding deadline", err)
		}
	}
}

func TestPassiveSuccessPostponesActiveButActivityAndPartialSignalsDoNot(t *testing.T) {
	c, _, ids, now := rig(t, 1)
	ids[0].InUse = true
	policy := Policy{UsageSeconds: 1800}
	ids[0].Passive = usageAt(*now)
	if _, err := c.Snapshot(ids, policy); err != nil {
		t.Fatal(err)
	}
	first := c.entries[ids[0].Key].UsageSchedule.DueAt
	*now = now.Add(4 * time.Minute)
	ids[0].Passive = usageAt(*now)
	if _, err := c.Snapshot(ids, policy); err != nil {
		t.Fatal(err)
	}
	second := c.entries[ids[0].Key].UsageSchedule.DueAt
	if !second.After(first) || !second.Equal(now.Add(activeUsageDelay(ids[0].Key, *now))) {
		t.Fatal("new real observation did not postpone fallback")
	}
	*now = now.Add(time.Minute)
	ids[0].Passive = &Usage{ObservedAt: *now, Windows: []Window{{Kind: "additional", ResetAt: now.Add(time.Hour)}}}
	if _, err := c.Snapshot(ids, policy); err != nil {
		t.Fatal(err)
	}
	if !c.entries[ids[0].Key].UsageSchedule.DueAt.Equal(second) {
		t.Fatal("additional-only data became useful main quota")
	}
	ids[0].Passive = nil
	*now = second
	calls := 0
	_, err := c.Step(context.Background(), ids, Request{Policy: policy}, func(_ context.Context, _ Identity, lane string) Result {
		calls++
		if lane != UsageLane {
			t.Fatal("activity created reset work")
		}
		return Result{Status: 200, Usage: usageAt(*now)}
	})
	if err != nil || calls != 1 {
		t.Fatal("activity without fresh quota postponed a due fallback", err)
	}
	third := c.entries[ids[0].Key].UsageSchedule.DueAt
	if !third.Equal(now.Add(activeUsageDelay(ids[0].Key, *now))) {
		t.Fatal("attempt did not begin a fresh stable scheduling epoch")
	}
}

func TestActiveDemotionRetainsOneDeadlineThenUsesNewClass(t *testing.T) {
	for _, likely := range []bool{false, true} {
		t.Run(fmt.Sprint(likely), func(t *testing.T) {
			c, _, ids, now := rig(t, 1)
			ids[0].InUse = true
			ids[0].Passive = usageAt(*now)
			policy := Policy{UsageSeconds: 1800}
			_, _ = c.Snapshot(ids, policy)
			due := c.entries[ids[0].Key].UsageSchedule.DueAt
			ids[0].InUse, ids[0].Active = false, likely
			*now = now.Add(time.Minute)
			snapshot, err := c.Snapshot(ids, policy)
			if err != nil || !snapshot.Accounts[0].UsageSchedule.DueAt.Equal(due) {
				t.Fatal("demotion slid an already-scheduled deadline", err)
			}
			*now = due
			_, err = c.Step(context.Background(), ids, Request{Policy: policy}, func(_ context.Context, _ Identity, _ string) Result {
				return Result{Status: 200, Usage: usageAt(*now)}
			})
			if err != nil {
				t.Fatal(err)
			}
			want := 24 * time.Hour
			if likely {
				want = 30 * time.Minute
			}
			if got := c.entries[ids[0].Key].UsageSchedule.DueAt.Sub(*now); got != want {
				t.Fatalf("demoted fallback = %s, want %s", got, want)
			}
		})
	}
}

func TestActivityCannotBypassManualOnlyCooldownsOrResetCadence(t *testing.T) {
	c, store, ids, now := rig(t, 4)
	for i := range ids {
		ids[i].Active, ids[i].InUse = true, true
	}
	_, _ = c.Snapshot(ids, Policy{})
	for _, e := range c.entries {
		e.UsageSchedule.DueAt = now.Add(-time.Hour)
		e.ResetSchedule.DueAt = now.Add(24 * time.Hour)
	}
	denied := func(context.Context, Identity, string) Result { t.Fatal("prohibited provider read"); return Result{} }
	if _, err := c.Step(context.Background(), ids, Request{}, denied); err != nil {
		t.Fatal(err)
	}
	ids[0].Blocked, ids[1].Enabled = true, false
	ids[2].RetryAt = now.Add(time.Hour)
	c.entries[ids[3].Key].UsageSchedule.RetryAt = now.Add(2 * time.Hour)
	if _, err := c.Step(context.Background(), ids, Request{Policy: Policy{UsageSeconds: 1800, ResetSeconds: 86400}}, denied); err != nil {
		t.Fatal(err)
	}
	if !store.state.AutomaticReset.NextStart.IsZero() {
		t.Fatal("activity modified the automatic-reset floor")
	}
}

func TestActiveAttemptDeadlineAndFailureFloorsSurviveRestart(t *testing.T) {
	c, store, ids, now := rig(t, 1)
	ids[0].InUse = true
	policy := Policy{UsageSeconds: 1800}
	_, _ = c.Snapshot(ids, policy)
	c.entries[ids[0].Key].UsageSchedule.DueAt = *now
	retry := now.Add(2 * time.Hour)
	_, err := c.Step(context.Background(), ids, Request{Policy: policy}, func(_ context.Context, _ Identity, _ string) Result {
		return Result{Status: 429, RetryAt: retry}
	})
	if err != nil {
		t.Fatal(err)
	}
	copy, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	copy.clock = c.clock
	*now = now.Add(15 * time.Minute)
	_, err = copy.Step(context.Background(), ids, Request{Policy: policy}, func(context.Context, Identity, string) Result {
		t.Fatal("active tier bypassed a persisted provider floor")
		return Result{}
	})
	if err != nil || !copy.entries[ids[0].Key].RetryAt.Equal(retry) {
		t.Fatal("restart lost account-wide backoff", err)
	}
}
