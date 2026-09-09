package executor

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
)

func TestUpstreamAttemptObserverIsOncePerTrackerAndInheritedByRetry(t *testing.T) {
	var calls atomic.Int64
	ctx := WithUpstreamAttemptObserver(context.Background(), func() { calls.Add(1) })
	MarkUpstreamAttempt(ctx) // no execution tracker: no observed attempt
	if calls.Load() != 0 {
		t.Fatal("observer ran without an execution tracker")
	}
	for i := 0; i < 2; i++ {
		ctx = WithUpstreamAttemptTracker(ctx)
		if UpstreamAttempted(ctx) {
			t.Fatal("new tracker inherited the previous attempted bit")
		}
		var workers sync.WaitGroup
		for j := 0; j < 32; j++ {
			workers.Go(func() { MarkUpstreamAttempt(ctx) })
		}
		workers.Wait()
		if !UpstreamAttempted(ctx) || calls.Load() != int64(i+1) {
			t.Fatalf("attempt tracker/observer mismatch: %d", calls.Load())
		}
	}
	MarkUpstreamAttempt(nil)
	MarkUpstreamAttempt(WithUpstreamAttemptObserver(WithUpstreamAttemptTracker(nil), nil))
}
