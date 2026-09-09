package codexmonitor

import (
	"fmt"
	"runtime"
	"sort"
	"testing"
	"time"
)

func TestMaximumPoolLocalSnapshotBudget(t *testing.T) {
	c, store, ids, now := rig(t, MaxAccounts)
	for i := range ids {
		ids[i].Active, ids[i].InUse = true, true
		ids[i].Passive = usageAt(*now)
	}
	if _, err := c.Snapshot(ids, Policy{}); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		bank := bankAt(*now)
		count, granted, expiry := 32, now.Add(-time.Hour), now.Add(24*time.Hour)
		bank.AvailableCount = &count
		for i := 0; i < count; i++ {
			bank.Credits = append(bank.Credits, Grant{ID: fmt.Sprintf("synthetic-%s-%d", id.AuthIndex, i),
				ResetType: "codex_rate_limits", Status: "available", GrantedAt: &granted, ExpiresAt: &expiry})
		}
		if err := c.ObserveRead(ids, id.Key, ResetLane, Result{Status: 200, Bank: bank}); err != nil {
			t.Fatal(err)
		}
	}
	writes, bytes := store.writes, store.bytes
	runtime.GC()
	var before, after, sample runtime.MemStats
	runtime.ReadMemStats(&before)
	peak := before.HeapAlloc
	elapsed := make([]time.Duration, 1000)
	for i := range elapsed {
		*now = now.Add(time.Second)
		start := time.Now()
		snapshot, err := c.Snapshot(ids, Policy{})
		elapsed[i] = time.Since(start)
		if err != nil || len(snapshot.Accounts) != MaxAccounts || snapshot.Pending != 0 {
			t.Fatal("local snapshot contract", err)
		}
		if i%10 == 0 {
			runtime.ReadMemStats(&sample)
			peak = max(peak, sample.HeapAlloc)
		}
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	sort.Slice(elapsed, func(i, j int) bool { return elapsed[i] < elapsed[j] })
	if elapsed[949] >= 250*time.Millisecond {
		t.Fatalf("snapshot p95 %s exceeds 250ms", elapsed[949])
	}
	if store.writes != writes || store.bytes != bytes {
		t.Fatal("cached redisplay wrote persistence")
	}
	if after.HeapAlloc > before.HeapAlloc+16<<20 {
		t.Fatal("retained heap growth exceeds one entire cache budget")
	}
	serialized := 0
	for _, value := range store.entries {
		serialized += len(value)
	}
	if serialized > 16<<20 {
		t.Fatal("cache exceeded 16MiB")
	}
	t.Logf("MONITOR_BUDGET accounts=128 grants=4096 snapshots=1000 p95=%s max=%s serialized_bytes=%d heap_before=%d sampled_peak=%d heap_after_gc=%d idle_writes=0", elapsed[949], elapsed[999], serialized, before.HeapAlloc, peak, after.HeapAlloc)
}

func TestExplicitReadAndActionBarrierSurviveRestart(t *testing.T) {
	c, store, ids, now := rig(t, 1)
	old := usageAt(*now)
	ids[0].Passive = old
	if err := c.ObserveRead(ids, ids[0].Key, ResetLane, Result{Status: 200, Bank: bankAt(*now)}); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Minute)
	if err := c.InvalidateAction(ids, ids[0].Key); err != nil {
		t.Fatal(err)
	}
	copy, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	copy.clock, copy.random = c.clock, c.random
	snapshot, err := copy.Snapshot(ids, Policy{})
	if err != nil || snapshot.Accounts[0].Usage != nil || snapshot.Accounts[0].Bank != nil {
		t.Fatal("restart revived pre-action values")
	}
	*now = now.Add(time.Minute)
	if err := copy.ObserveRead(ids, ids[0].Key, UsageLane, Result{Status: 200, Usage: usageAt(*now)}); err != nil {
		t.Fatal(err)
	}
	snapshot, _ = copy.Snapshot(ids, Policy{})
	if snapshot.Accounts[0].Usage == nil || snapshot.Accounts[0].Bank != nil {
		t.Fatal("fresh read crossed lanes")
	}
	store.fail = true
	if err := copy.InvalidateAction(ids, ids[0].Key); err == nil {
		t.Fatal("invalidation failed open on disk failure")
	}
}
