package codexmonitor

import (
	"context"
	"testing"
	"time"
)

func TestUsageRefreshRetainsTenCooldownIntentsAndDrainsWithoutResetReads(t *testing.T) {
	c, _, ids, now := rig(t, 10)
	_, _ = c.Snapshot(ids, Policy{})
	for _, id := range ids {
		e := c.entries[id.Key]
		e.Usage = usageAt(now.Add(-time.Hour))
		e.Bank = bankAt(*now)
		e.UsageSchedule.Error = "oauth_rejected"
		e.UsageSchedule.RetryAt = now.Add(30 * time.Minute)
		e.RetryAt = e.UsageSchedule.RetryAt
	}
	bankTime := *now
	calls := 0
	fetch := func(_ context.Context, _ Identity, kind string) Result {
		if kind != UsageLane {
			t.Fatal("usage refresh fetched resets")
		}
		calls++
		return Result{Status: 200, Usage: usageAt(*now)}
	}
	for range 100 {
		s, err := c.Step(context.Background(), ids, Request{Refresh: UsageLane}, fetch)
		if err != nil || s.Pending != 10 || calls != 0 {
			t.Fatal("cooldown intent lost or bypassed", err, s.Pending)
		}
	}
	*now = now.Add(30 * time.Minute)
	for range 10 {
		_, err := c.Step(context.Background(), ids, Request{}, fetch)
		if err != nil {
			t.Fatal(err)
		}
		*now = now.Add(11 * time.Second)
	}
	s, _ := c.Snapshot(ids, Policy{})
	if calls != 10 || s.Pending != 0 {
		t.Fatal("one explicit batch did not drain", calls, s.Pending)
	}
	for _, a := range s.Accounts {
		if a.UsageSchedule.Error != "" || a.Usage == nil || !a.Usage.ObservedAt.After(bankTime) || !a.Bank.ObservedAt.Equal(bankTime) {
			t.Fatal("fresh usage or independent reset history was lost")
		}
	}
}
