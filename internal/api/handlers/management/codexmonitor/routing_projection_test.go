package codexmonitor

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRoutingProjectionMergesScopesWithoutStorageOrProviderWork(t *testing.T) {
	c, store, ids, now := rig(t, 1)
	ids[0].Passive = usageAt(*now)
	if _, err := c.Snapshot(ids, Policy{}); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Minute)
	short := &Usage{ObservedAt: *now, Source: "passive", Windows: []Window{{Kind: "five_hour", Label: "5 hour", UsedPercent: 100,
		DurationSeconds: 18000, ResetAt: now.Add(5 * time.Hour)}}}
	writes, bytes := store.writes, store.bytes
	store.fail = true
	for i := 0; i < 100; i++ {
		projection := c.RoutingProjection(ids[0].Key, short, *now)
		if !projection.Available || projection.Weekly == nil || projection.Weekly.UsedPercent != 28 ||
			projection.Short == nil || projection.Short.UsedPercent != 100 || !projection.ShortObservedAt.Equal(*now) {
			t.Fatalf("independent scopes were lost: %+v", projection)
		}
		// Callers receive copies, not mutable pointers into the cache.
		projection.Weekly.UsedPercent = 99
	}
	if store.writes != writes || store.bytes != bytes || c.inFlight {
		t.Fatal("routing projection performed observation or storage work")
	}
}

func TestRoutingProjectionUnknownStaleAndAdditionalAreNotBaseExhaustion(t *testing.T) {
	c, _, ids, now := rig(t, 1)
	for _, tc := range []struct {
		name  string
		usage *Usage
	}{
		{name: "absent"},
		{name: "additional only", usage: &Usage{ObservedAt: *now, Windows: []Window{{Kind: "additional", DurationSeconds: 604800, UsedPercent: 100, ResetAt: now.Add(7 * 24 * time.Hour)}}}},
		{name: "stale positive", usage: usageAt(now.Add(-49 * time.Hour))},
		{name: "future observation", usage: usageAt(now.Add(time.Minute))},
		{name: "expired weekly", usage: usageAt(now.Add(-8 * 24 * time.Hour))},
		{name: "wrong base duration", usage: &Usage{ObservedAt: *now, Windows: []Window{{Kind: "weekly", DurationSeconds: 18000, UsedPercent: 100, ResetAt: now.Add(5 * time.Hour)}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			projection := c.RoutingProjection(ids[0].Key, tc.usage, *now)
			if !projection.Available || projection.Weekly != nil || projection.Short != nil {
				t.Fatalf("invented base quota: %+v", projection)
			}
		})
	}
	stale := usageAt(now.Add(-49 * time.Hour))
	stale.Windows[0].UsedPercent = 100
	projection := c.RoutingProjection(ids[0].Key, stale, *now)
	if projection.Weekly == nil || projection.Weekly.UsedPercent != 100 {
		t.Fatal("unexpired confirmed weekly exclusion expired with display freshness")
	}
}

func TestRoutingWeeklyExhaustionSurvivesPartialReadInvalidationAndRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	now := time.Now().UTC()
	c.clock = func() time.Time { return now }
	usage := usageAt(now)
	usage.Windows[0].UsedPercent = 100
	reset := usage.Windows[0].ResetAt
	key := fmt.Sprintf("%064x", 1)
	ids := []Identity{{Key: key, AuthIndex: "synthetic", Enabled: true, Passive: usage}}
	if _, err := c.Snapshot(ids, Policy{}); err != nil {
		t.Fatal(err)
	}
	// No family has read the projection yet. The existing observation owner
	// must preserve exhaustion before a newer partial reading replaces Usage.
	now = now.Add(time.Minute)
	ids[0].Passive = &Usage{ObservedAt: now, Source: "passive", Plan: "pro", Windows: []Window{{Kind: "five_hour", Label: "5 hour", DurationSeconds: 18000, UsedPercent: 5, ResetAt: now.Add(5 * time.Hour)}}}
	if _, err := c.Snapshot(ids, Policy{}); err != nil {
		t.Fatal(err)
	}
	if err := c.InvalidateAction(ids, key); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	ids[0].Passive = usageAt(now)
	ids[0].Passive.Windows[0].UsedPercent = 0
	ids[0].Passive.Windows[0].ResetAt = reset
	if _, err := c.Snapshot(ids, Policy{}); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	// The unchanged strict v1 cache reader accepts the same entry/control
	// shapes; safety evidence is a sibling rather than an added entry field.
	if _, entries, err := store.load(); err != nil || entries[key].Usage.Windows[0].UsedPercent != 0 {
		t.Fatalf("legacy v1 cache compatibility: %v", err)
	}
	c, err = New(store)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	c.clock = func() time.Time { return now }
	projection := c.RoutingProjection(key, nil, now)
	if !projection.Available || projection.Weekly == nil || projection.Weekly.UsedPercent != 100 || !projection.Weekly.ResetAt.Equal(reset) {
		t.Fatalf("new partial/positive readings or restart erased exhaustion: %+v", projection)
	}
	if other := c.RoutingProjection(fmt.Sprintf("%064x", 2), nil, now); !other.Available || other.Weekly != nil {
		t.Fatal("replacement identity inherited exhaustion")
	}
	now = reset.Add(time.Second)
	if expired := c.RoutingProjection(key, nil, now); expired.Weekly != nil {
		t.Fatal("elapsed weekly window remained excluded")
	}
}

func TestRoutingWeeklyCorruptCompanionFailsClosedAndPreservesBytes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	path := dir + ".routing-weekly.v1.json"
	data := []byte(`{"schema":1,"schema":2,"windows":{}}`)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c, err := New(store); err == nil {
		_ = c.Close()
		t.Fatal("accepted ambiguous routing safety state")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(data) {
		t.Fatal("modified corrupt safety evidence")
	}
	// Constructor failure releases the existing monitor owner's lock.
	store, err = OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
}
