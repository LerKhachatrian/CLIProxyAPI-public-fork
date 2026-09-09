package codexmonitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func makeResetsDue(t *testing.T, c *Coordinator, ids []Identity, now time.Time) {
	t.Helper()
	if _, err := c.Snapshot(ids, Policy{}); err != nil {
		t.Fatal(err)
	}
	for _, e := range c.entries {
		e.ResetSchedule.DueAt = now.Add(-time.Hour)
		c.dirty[e.Key] = true
	}
}

func TestAutomaticResetJitterIsSampledOnceAndDoesNotBlockOtherLanes(t *testing.T) {
	for _, endpoint := range []string{"minimum", "midpoint", "maximum"} {
		t.Run(endpoint, func(t *testing.T) {
			c, store, ids, now := rig(t, 4)
			c.random = func(n int64) int64 {
				if endpoint == "minimum" {
					return 0
				}
				if endpoint == "maximum" {
					return n - 1
				}
				return n / 2
			}
			makeResetsDue(t, c, ids, *now)
			start := *now
			calls := 0
			fetch := func(_ context.Context, _ Identity, kind string) Result {
				calls++
				if calls == 1 && !store.state.AutomaticReset.valid() {
					t.Fatal("automatic start reached provider before durable pacing")
				}
				if kind == UsageLane {
					return Result{Status: 200, Usage: usageAt(*now)}
				}
				return Result{Status: 200, Bank: bankAt(*now)}
			}
			policy := Policy{ResetSeconds: 86400, GapSeconds: 10}
			if _, err := c.Step(context.Background(), ids, Request{Policy: policy}, fetch); err != nil {
				t.Fatal(err)
			}
			deadline := store.state.AutomaticReset.NextStart
			expected := map[string]time.Duration{"minimum": time.Minute, "midpoint": 90 * time.Second, "maximum": 2*time.Minute - time.Nanosecond}[endpoint]
			if calls != 1 || deadline.Sub(start) != expected {
				t.Fatalf("chosen interval: %v, calls %d", deadline.Sub(start), calls)
			}
			// Repeated clients/views never redraw the chosen interval.
			for i := 0; i < 100; i++ {
				*now = start.Add(10 * time.Second)
				if _, err := c.Step(context.Background(), ids, Request{Policy: policy}, fetch); err != nil {
					t.Fatal(err)
				}
				if calls != 1 || !c.state.AutomaticReset.NextStart.Equal(deadline) {
					t.Fatal("redrew or bypassed automatic reset pacing")
				}
			}
			// Explicit usage and reset queues keep the original ten-second gap.
			if _, err := c.Step(context.Background(), ids, Request{Policy: policy, Refresh: UsageLane, AuthIndex: ids[1].AuthIndex}, fetch); err != nil {
				t.Fatal(err)
			}
			*now = start.Add(20 * time.Second)
			if _, err := c.Step(context.Background(), ids, Request{Policy: policy, Refresh: ResetLane, AuthIndex: ids[2].AuthIndex}, fetch); err != nil {
				t.Fatal(err)
			}
			if calls != 3 || !c.state.AutomaticReset.NextStart.Equal(deadline) {
				t.Fatal("automatic floor blocked manual work or manual work redrew floor")
			}
			*now = deadline.Add(-time.Nanosecond)
			if _, err := c.Step(context.Background(), ids, Request{Policy: policy}, fetch); err != nil || calls != 3 {
				t.Fatal("automatic reset started before its exact floor", err)
			}
			*now = deadline
			if _, err := c.Step(context.Background(), ids, Request{Policy: policy}, fetch); err != nil || calls != 4 {
				t.Fatal("eligible automatic reset did not resume", err)
			}
		})
	}
}

func TestAutomaticResetFloorSurvivesFailureRestartAndAccountRemoval(t *testing.T) {
	c, store, ids, now := rig(t, 3)
	makeResetsDue(t, c, ids, *now)
	calls := 0
	fetch := func(context.Context, Identity, string) Result { calls++; return Result{Status: 503} }
	policy := Policy{ResetSeconds: 604800}
	if _, err := c.Step(context.Background(), ids, Request{Policy: policy}, fetch); err != nil {
		t.Fatal(err)
	}
	deadline := store.state.AutomaticReset.NextStart
	claimed := c.entries[ids[0].Key].ResetSchedule
	if claimed.DueAt.Sub(claimed.LastAttempt) < 7*24*time.Hour {
		t.Fatal("weekly cadence was shortened")
	}
	c, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	c.clock = func() time.Time { return *now }
	c.random = func(n int64) int64 { return n - 1 }
	ids = ids[1:] // removing the last attempted account cannot remove the shared floor
	*now = deadline.Add(-time.Second)
	if _, err := c.Step(context.Background(), ids, Request{Policy: policy}, fetch); err != nil || calls != 1 {
		t.Fatal("restart/removal/failure erased the automatic reset floor", err)
	}
	*now = deadline
	if _, err := c.Step(context.Background(), ids, Request{Policy: policy}, fetch); err != nil || calls != 2 {
		t.Fatal("next eligible account was starved", err)
	}
}

func TestAutomaticResetMaximumCatchupAndMixedManualQueues(t *testing.T) {
	for _, manual := range []bool{false, true} {
		c, _, ids, now := rig(t, MaxAccounts)
		c.random = func(n int64) int64 { return n - 1 }
		makeResetsDue(t, c, ids, *now)
		start := *now
		counts := map[string]int{}
		policy := Policy{ResetSeconds: 86400, GapSeconds: 60}
		fetch := func(_ context.Context, _ Identity, kind string) Result {
			counts[kind]++
			if kind == UsageLane {
				return Result{Status: 200, Usage: usageAt(*now)}
			}
			return Result{Status: 200, Bank: bankAt(*now)}
		}
		for step := 0; step < 2*MaxAccounts+2; step++ {
			req := Request{Policy: policy}
			if manual && step == 0 {
				req.Refresh = UsageLane
			} else if manual && step == 1 {
				req.Refresh = ResetLane
			}
			snapshot, err := c.Step(context.Background(), ids, req, fetch)
			if err != nil {
				t.Fatal(err)
			}
			for _, account := range snapshot.Accounts {
				if account.UsageSchedule.Error == "refresh_expired" || account.ResetSchedule.Error == "refresh_expired" {
					t.Fatal("automatic spacing expired the bounded manual queues")
				}
			}
			*now = now.Add(time.Minute)
		}
		if now.Sub(start) >= MaxPendingAge {
			t.Fatal("test exceeded the existing queue envelope")
		}
		if !manual && (counts[ResetLane] != MaxAccounts || counts[UsageLane] != 0) {
			t.Fatalf("automatic catchup was unfair: %+v", counts)
		}
		if manual && (counts[UsageLane] != MaxAccounts || counts[ResetLane] != MaxAccounts+1) {
			t.Fatalf("independent manual queues were starved: %+v", counts)
		}
	}
}

func TestAutomaticResetFileStoreCompatibilityAndNoExtraWrites(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "monitor")
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	initialStore := s
	t.Cleanup(func() { _ = initialStore.Close() })
	now := time.Date(2026, 9, 9, 4, 0, 0, 0, time.UTC)
	state := control{Schema: 1, NextStart: now.Add(10 * time.Second), Starts: []time.Time{now},
		AutomaticReset: automaticResetControl{Schema: 1, LastStart: now, NextStart: now.Add(90 * time.Second)}}
	if err := s.saveControl(state); err != nil {
		t.Fatal(err)
	}
	// Exact original v1 shape: a strict old reader accepts this control file.
	legacy := struct {
		Schema    int         `json:"schema"`
		NextStart time.Time   `json:"next_start"`
		Starts    []time.Time `json:"starts"`
	}{}
	data, _ := os.ReadFile(filepath.Join(dir, "control.json"))
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&legacy); err != nil || legacy.Schema != 1 {
		t.Fatal("broke strict legacy control reader", err)
	}
	files, _ := os.ReadDir(dir)
	if len(files) != 2 { // control + existing OS lock; companion is outside
		t.Fatal("introduced a file an older directory scanner rejects")
	}
	companion := s.automaticResetPath()
	fixed := now.Add(-24 * time.Hour)
	if err := os.Chtimes(companion, fixed, fixed); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		state.NextStart = state.NextStart.Add(10 * time.Second)
		if err := s.saveControl(state); err != nil {
			t.Fatal(err)
		}
	}
	info, _ := os.Stat(companion)
	if !info.ModTime().Equal(fixed) || info.Size() > 4096 {
		t.Fatal("unchanged reset floor was rewritten or exceeded its size budget")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	restored, _, err := s.load()
	if err != nil || restored.AutomaticReset != state.AutomaticReset {
		t.Fatal("restart lost independent reset pacing", err)
	}
}

func TestAutomaticResetFileFailuresArePreservedAndFailClosed(t *testing.T) {
	for name, raw := range map[string]string{
		"malformed": "{", "oversized": string(bytes.Repeat([]byte("x"), 4097)),
		"future": `{"schema":2,"last_start":"2026-09-09T04:00:00Z","next_start":"2026-09-09T04:01:30Z"}`,
		"short":  `{"schema":1,"last_start":"2026-09-09T04:00:00Z","next_start":"2026-09-09T04:00:59Z"}`,
		"long":   `{"schema":1,"last_start":"2026-09-09T04:00:00Z","next_start":"2026-09-09T04:02:01Z"}`,
		"zero":   `{"schema":1}`, "duplicate": `{"schema":1,"schema":1}`,
		"unknown": `{"schema":1,"last_start":"2026-09-09T04:00:00Z","next_start":"2026-09-09T04:01:30Z","unexpected":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "monitor")
			s, err := OpenStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			path := s.automaticResetPath()
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			if c, err := New(s); err == nil {
				_ = c.Close()
				t.Fatal("accepted invalid automatic reset state")
			}
			data, _ := os.ReadFile(path)
			if string(data) != raw {
				t.Fatal("modified rejected state")
			}
		})
	}
	for _, blockedFile := range []string{"companion", "control"} {
		t.Run("write-"+blockedFile, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "monitor")
			s, err := OpenStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			c, err := New(s)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			_, _, ids, now := rig(t, 2)
			c.clock = func() time.Time { return *now }
			makeResetsDue(t, c, ids, *now)
			path := s.automaticResetPath()
			if blockedFile == "control" {
				path = filepath.Join(dir, "control.json")
			}
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
			_, err = c.Step(context.Background(), ids, Request{Policy: Policy{ResetSeconds: 86400}}, func(context.Context, Identity, string) Result {
				t.Error("provider reached despite failed safety-state write")
				return Result{}
			})
			if err == nil {
				t.Fatal("accepted failed safety-state write")
			}
			if blockedFile == "control" {
				var persisted automaticResetControl
				if err := readJSON(s.automaticResetPath(), 4096, &persisted); err != nil || !persisted.valid() {
					t.Fatal("partial control failure lost the persisted pacing floor", err)
				}
			}
			if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
				t.Fatal("deleted conflicting state")
			}
		})
	}
}
