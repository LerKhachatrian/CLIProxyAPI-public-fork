package codexmonitor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFileStoreExclusiveRestartAndInterruptedWrite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "monitor")
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if other, err := OpenStore(dir); err == nil {
		_ = other.Close()
		t.Fatal("two cache owners")
	}
	c, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	ids := []Identity{{Key: fmt.Sprintf("%064x", 1), AuthIndex: "synthetic", Enabled: true, Active: true, Passive: usageAt(now)}}
	snapshot, err := c.Step(context.Background(), ids, Request{Refresh: ResetLane}, func(context.Context, Identity, string) Result {
		return Result{Status: 200, Bank: bankAt(time.Now().UTC())}
	})
	if err != nil || snapshot.Accounts[0].Bank == nil {
		t.Fatalf("store step: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".write-123.tmp"), []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	c, err = New(store)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	snapshot, err = c.Snapshot(ids, Policy{})
	if err != nil || snapshot.Accounts[0].Bank == nil || snapshot.Accounts[0].ResetSchedule.LastAttempt.IsZero() {
		t.Fatal("restart lost cold inventory or attempt")
	}
	if _, err := os.Stat(filepath.Join(dir, ".write-123.tmp")); !os.IsNotExist(err) {
		t.Fatal("orphan write not recovered")
	}
}

func TestFileStoreCorruptFutureAndUnknownFilesFailClosed(t *testing.T) {
	for _, test := range []struct{ name, file, data string }{
		{"corrupt", "control.json", "{"},
		{"future", "control.json", `{"schema":2,"next_start":"0001-01-01T00:00:00Z","starts":[]}`},
		{"unknown", "unrecognized.txt", "do not delete"},
		{"duplicate", "control.json", `{"schema":1,"schema":2}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, test.file)
			if err := os.WriteFile(path, []byte(test.data), 0600); err != nil {
				t.Fatal(err)
			}
			store, err := OpenStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			if c, err := New(store); err == nil {
				_ = c.Close()
				t.Fatal("accepted corrupt or unknown cache")
			}
			data, _ := os.ReadFile(path)
			if string(data) != test.data {
				t.Fatal("modified unrecognized state")
			}
		})
	}
}

func TestFileStoreRefusesRelativePathsAndUnsafeKeys(t *testing.T) {
	if s, err := OpenStore("relative"); err == nil {
		_ = s.Close()
		t.Fatal("relative store")
	}
	s, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.saveEntry(&Entry{Schema: 1, Key: "../escape"}); err == nil {
		t.Fatal("unsafe key")
	}
	if err := s.removeEntry("../escape"); err == nil {
		t.Fatal("unsafe delete")
	}
}
