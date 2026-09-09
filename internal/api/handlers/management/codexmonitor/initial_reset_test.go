package codexmonitor

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestInitialResetInventoryFreshAndLegacy(t *testing.T) {
	for _, count := range []int{10, MaxAccounts} {
		for _, legacy := range []bool{false, true} {
			for _, seconds := range []int{86400, 604800} {
				t.Run(fmt.Sprintf("count=%d/legacy=%t/interval=%d", count, legacy, seconds), func(t *testing.T) {
					c, store, ids, now := rig(t, count)
					c.random = func(n int64) int64 { return n - 1 }
					if legacy {
						for _, id := range ids {
							if err := store.saveEntry(&Entry{Schema: 1, Key: id.Key,
								ResetSchedule: Lane{DueAt: now.Add(23 * time.Hour)}}); err != nil {
								t.Fatal(err)
							}
						}
						var err error
						c, err = New(store)
						if err != nil {
							t.Fatal(err)
						}
						c.clock = func() time.Time { return *now }
						c.random = func(n int64) int64 { return n - 1 }
					}
					calls, seen := 0, map[string]bool{}
					fetch := func(_ context.Context, id Identity, kind string) Result {
						if kind != ResetLane || seen[id.Key] {
							t.Fatal("initial inventory fetched usage or repeated an account")
						}
						seen[id.Key], calls = true, calls+1
						_, durable, err := store.load()
						if err != nil || durable[id.Key].ResetSchedule.LastAttempt.IsZero() {
							t.Fatal("initial attempt was not persisted before dispatch", err)
						}
						return Result{Status: 200, Bank: bankAt(*now)}
					}
					policy := Policy{ResetSeconds: seconds, GapSeconds: 10}
					start := *now
					for i := range ids {
						snapshot, err := c.Step(context.Background(), ids, Request{Policy: policy}, fetch)
						if err != nil || calls != i+1 || snapshot.Attempted != ResetLane || snapshot.Pending != 0 {
							t.Fatalf("initial inventory must be eligible without a manual intent: calls=%d step=%d error=%v", calls, i, err)
						}
						deadline := c.state.AutomaticReset.NextStart
						if gap := deadline.Sub(*now); gap < time.Minute || gap > 2*time.Minute {
							t.Fatalf("initial inventory bypassed automatic spacing: %v", gap)
						}
						*now = deadline.Add(-time.Nanosecond)
						if _, err := c.Step(context.Background(), ids, Request{Policy: policy}, fetch); err != nil || calls != i+1 {
							t.Fatal("initial inventory started before the shared floor", err)
						}
						*now = deadline
					}
					if calls != len(ids) || now.Sub(start) > time.Duration(count)*2*time.Minute {
						t.Fatal("initial inventory retained the old all-day wait")
					}
					for _, e := range c.entries {
						if delay := e.ResetSchedule.DueAt.Sub(e.ResetSchedule.LastAttempt); delay < time.Duration(seconds)*time.Second || delay >= time.Duration(float64(seconds)*float64(time.Second)*13/12) {
							t.Fatalf("subsequent cadence changed: %v", delay)
						}
					}
				})
			}
		}
	}
}

func TestInitialResetInventoryManualOnlyAndNoIdleWrites(t *testing.T) {
	c, store, ids, now := rig(t, MaxAccounts)
	c.random = func(n int64) int64 { return n - 1 }
	calls := 0
	fetch := func(context.Context, Identity, string) Result { calls++; return Result{Status: 503} }
	for i := 0; i < 1000; i++ {
		if _, err := c.Step(context.Background(), ids, Request{}, fetch); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(30 * time.Second)
	}
	if calls != 0 {
		t.Fatal("Manual only performed an initial provider check")
	}
	writes := store.writes
	for i := 0; i < 1000; i++ {
		if _, err := c.Snapshot(ids, Policy{ResetSeconds: 86400}); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(time.Second)
	}
	if calls != 0 || store.writes != writes {
		t.Fatal("local views performed provider reads or repeated initial-schedule writes")
	}
	if _, err := c.Step(context.Background(), ids, Request{Policy: Policy{ResetSeconds: 86400}}, fetch); err != nil || calls != 1 {
		t.Fatal("enabling Auto did not begin paced inventory", err)
	}
}

func TestInitialResetInventoryPreservesSafetyAndExistingObservations(t *testing.T) {
	c, store, ids, now := rig(t, 8)
	for _, id := range ids {
		if err := store.saveEntry(&Entry{Schema: 1, Key: id.Key,
			ResetSchedule: Lane{DueAt: now.Add(23 * time.Hour)}}); err != nil {
			t.Fatal(err)
		}
	}
	_, entries, _ := store.load()
	ids[0].Enabled = false
	ids[1].Blocked = true
	ids[2].RetryAt = now.Add(time.Hour)
	entries[ids[3].Key].RetryAt = now.Add(time.Hour)
	entries[ids[4].Key].ResetSchedule.RetryAt = now.Add(time.Hour)
	entries[ids[5].Key].ResetSchedule.LastAttempt = now.Add(-time.Hour)
	entries[ids[5].Key].ResetSchedule.Error = "provider_unavailable"
	entries[ids[6].Key].Bank = bankAt(now.Add(-time.Hour)) // observed valid zero is not missing
	for _, entry := range entries {
		if err := store.saveEntry(entry); err != nil {
			t.Fatal(err)
		}
	}
	var err error
	c, err = New(store)
	if err != nil {
		t.Fatal(err)
	}
	c.clock = func() time.Time { return *now }
	calls := 0
	fetch := func(_ context.Context, id Identity, _ string) Result {
		calls++
		if id.Key != ids[7].Key {
			t.Fatal("initial refresh bypassed exclusion, attempt cadence or existing inventory")
		}
		return Result{Status: 403, RetryAt: now.Add(48 * time.Hour)}
	}
	policy := Policy{ResetSeconds: 86400}
	if _, err := c.Step(context.Background(), ids, Request{Policy: policy}, fetch); err != nil || calls != 1 {
		t.Fatal("eligible unknown account was not selected", err)
	}
	attempted := c.entries[ids[7].Key].ResetSchedule
	observedDue := c.entries[ids[6].Key].ResetSchedule.DueAt
	c, err = New(store)
	if err != nil {
		t.Fatal(err)
	}
	c.clock = func() time.Time { return *now }
	*now = now.Add(26 * time.Hour)
	if _, err := c.Step(context.Background(), ids[7:], Request{Policy: policy}, fetch); err != nil || calls != 1 {
		t.Fatal("restart replayed a failed first attempt or bypassed Retry-After", err)
	}
	if !c.entries[ids[7].Key].ResetSchedule.LastAttempt.Equal(attempted.LastAttempt) || !observedDue.Equal(attempted.LastAttempt.Add(23*time.Hour)) {
		t.Fatal("attempt history or existing inventory schedule changed")
	}
}

func TestInitialResetInventoryLegacyFileUpgradeAndPersistenceFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("fail=%t", fail), func(t *testing.T) {
			_, _, ids, now := rig(t, 1)
			dir := filepath.Join(t.TempDir(), "monitor")
			file, err := OpenStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			legacy := &Entry{Schema: 1, Key: ids[0].Key, ResetSchedule: Lane{DueAt: now.Add(23 * time.Hour)}}
			if err := file.saveEntry(legacy); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			file, err = OpenStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			var store Store = file
			if fail {
				store = &failInitialSaveStore{Store: file}
			}
			c, err := New(store)
			if err != nil {
				t.Fatal(err)
			}
			c.clock = func() time.Time { return *now }
			calls := 0
			fetch := func(context.Context, Identity, string) Result {
				calls++
				return Result{Status: 200, Bank: bankAt(*now)}
			}
			_, err = c.Step(context.Background(), ids, Request{Policy: Policy{ResetSeconds: 86400}}, fetch)
			if fail {
				if err == nil || calls != 0 {
					t.Fatal("failed schedule persistence reached provider")
				}
			} else if err != nil || calls != 1 {
				t.Fatal("strict legacy cache did not receive initial inventory", err)
			}
			_, after, err := file.load()
			if err != nil || after[ids[0].Key].Schema != 1 {
				t.Fatal("initial inventory broke strict v1 cache", err)
			}
			if fail && !after[ids[0].Key].ResetSchedule.DueAt.Equal(legacy.ResetSchedule.DueAt) {
				t.Fatal("failed write changed the persisted legacy schedule")
			}
		})
	}
}

type failInitialSaveStore struct{ Store }

func (s *failInitialSaveStore) saveEntry(*Entry) error {
	return fmt.Errorf("injected initial schedule persistence failure")
}
