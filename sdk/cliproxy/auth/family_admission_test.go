package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

type familyAcquireResult struct {
	lease *familyAdmissionLease
	err   error
}

func waitFamilyState(t *testing.T, r *familyRouter, predicate func(FamilyRoutingStatus) bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for {
		r.mu.Lock()
		changed := r.changed
		r.mu.Unlock()
		if status := r.status(); predicate(status) {
			return
		}
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatalf("admission transition not reached: %+v", r.status())
		}
	}
}

func startFamilyAcquire(ctx context.Context, r *familyRouter, token *familySelection) <-chan familyAcquireResult {
	result := make(chan familyAcquireResult, 1)
	go func() { lease, err := r.acquire(ctx, token); result <- familyAcquireResult{lease, err} }()
	return result
}

func awaitFamilyAcquire(t *testing.T, result <-chan familyAcquireResult) familyAcquireResult {
	t.Helper()
	select {
	case acquired := <-result:
		return acquired
	case <-time.After(5 * time.Second):
		t.Fatal("admission did not complete")
		return familyAcquireResult{}
	}
}

func TestFamilyAdmissionFairQueueBoundsAndCancellation(t *testing.T) {
	f := newFamilyFixture(t, 1, FamilyRoutingConfig{MaxConcurrentPerAccount: 1, MaxQueuedPerAccount: 3})
	_, optsA := f.pick(t, "large", "large", "", "model")
	_, optsB := f.pick(t, "small", "small", "", "model")
	a, b := familyTokenFromOptions(optsA), familyTokenFromOptions(optsB)
	active, err := f.router.acquire(t.Context(), a)
	if err != nil {
		t.Fatal(err)
	}
	defer active.release()
	first := startFamilyAcquire(t.Context(), f.router, a)
	waitFamilyState(t, f.router, func(s FamilyRoutingStatus) bool { return s.Queued == 1 })
	second := startFamilyAcquire(t.Context(), f.router, a)
	waitFamilyState(t, f.router, func(s FamilyRoutingStatus) bool { return s.Queued == 2 })
	small := startFamilyAcquire(t.Context(), f.router, b)
	waitFamilyState(t, f.router, func(s FamilyRoutingStatus) bool { return s.Queued == 3 })
	if _, err := f.router.acquire(t.Context(), b); statusCodeFromError(err) != http.StatusServiceUnavailable {
		t.Fatalf("queue bound was not enforced: %v", err)
	}
	active.release()
	firstLease := awaitFamilyAcquire(t, first)
	if firstLease.err != nil {
		t.Fatal(firstLease.err)
	}
	firstLease.lease.release()
	// Dispatch rotates the large family's remaining request behind the small
	// family, rather than letting the large FIFO consume every next slot.
	smallLease := awaitFamilyAcquire(t, small)
	if smallLease.err != nil {
		t.Fatal(smallLease.err)
	}
	select {
	case <-second:
		t.Fatal("large family monopolized the next slot")
	default:
	}
	smallLease.lease.release()
	secondLease := awaitFamilyAcquire(t, second)
	if secondLease.err != nil {
		t.Fatal(secondLease.err)
	}
	secondLease.lease.release()

	activeCtx, cancelActive := context.WithCancel(t.Context())
	active, err = f.router.acquire(activeCtx, a)
	if err != nil {
		t.Fatal(err)
	}
	queuedCtx, cancelQueued := context.WithCancel(t.Context())
	queued := startFamilyAcquire(queuedCtx, f.router, b)
	waitFamilyState(t, f.router, func(s FamilyRoutingStatus) bool { return s.Queued == 1 })
	cancelQueued()
	if result := awaitFamilyAcquire(t, queued); !errors.Is(result.err, context.Canceled) {
		t.Fatalf("queued cancellation: %v", result.err)
	}
	cancelActive()
	waitFamilyState(t, f.router, func(s FamilyRoutingStatus) bool { return s.InFlight == 0 && s.Queued == 0 })
	active.release()
	f.router.mu.Lock()
	retained := len(f.router.accounts)
	f.router.mu.Unlock()
	if retained != 0 {
		t.Fatal("idle admission account was retained")
	}
}

func TestFamilyAdmissionResetAndFailureNeverRetireActiveStream(t *testing.T) {
	f := newFamilyFixture(t, 1, FamilyRoutingConfig{MaxConcurrentPerAccount: 1})
	a, opts := f.pick(t, "root", "root", "", "model")
	old := familyTokenFromOptions(opts)
	active, err := f.router.acquire(t.Context(), old)
	if err != nil {
		t.Fatal(err)
	}
	defer active.release()
	queued := startFamilyAcquire(t.Context(), f.router, old)
	waitFamilyState(t, f.router, func(s FamilyRoutingStatus) bool { return s.Queued == 1 })
	f.router.resetAssignments()
	if result := awaitFamilyAcquire(t, queued); !isFamilyReselect(result.err) {
		t.Fatalf("stale waiter survived reset: %v", result.err)
	}
	if status := f.router.status(); status.InFlight != 1 || status.Queued != 0 {
		t.Fatalf("reset released a running stream: %+v", status)
	}
	if err := active.validate(); !isFamilyReselect(err) {
		t.Fatalf("old transport could reconnect after reset: %v", err)
	}
	_, nextOpts := f.pick(t, "root", "child", "root", "model")
	fresh := familyTokenFromOptions(nextOpts)
	f.router.onResult(old, Result{AuthID: a.ID, Error: &Error{HTTPStatus: http.StatusServiceUnavailable}})
	next := startFamilyAcquire(t.Context(), f.router, fresh)
	waitFamilyState(t, f.router, func(s FamilyRoutingStatus) bool { return s.Queued == 1 })
	active.release()
	result := awaitFamilyAcquire(t, next)
	if result.err != nil {
		t.Fatal(result.err)
	}
	result.lease.release()
}

func TestFamilyAdmissionRechecksWeeklyQuotaBeforeEveryTransportAttempt(t *testing.T) {
	f := newFamilyFixture(t, 2, FamilyRoutingConfig{})
	_, opts := f.pick(t, "root", "root", "", "model")
	lease, err := f.router.acquire(t.Context(), familyTokenFromOptions(opts))
	if err != nil {
		t.Fatal(err)
	}
	defer lease.release()
	if err := lease.validate(); err != nil {
		t.Fatal(err)
	}
	f.exhaust(0)
	if err := lease.validate(); !isFamilyReselect(err) {
		t.Fatalf("confirmed weekly exhaustion admitted retry: %v", err)
	}
	next, _ := f.pick(t, "root", "grandchild", "child", "model")
	if next.ID == f.auths[0].ID {
		t.Fatal("family failover reused exhausted account")
	}
}

func TestFamilyAdmissionStopReleasesWaitersAndLifetimeLock(t *testing.T) {
	f := newFamilyFixture(t, 1, FamilyRoutingConfig{MaxConcurrentPerAccount: 1})
	_, opts := f.pick(t, "root", "root", "", "model")
	active, err := f.router.acquire(t.Context(), familyTokenFromOptions(opts))
	if err != nil {
		t.Fatal(err)
	}
	queued := startFamilyAcquire(t.Context(), f.router, familyTokenFromOptions(opts))
	waitFamilyState(t, f.router, func(s FamilyRoutingStatus) bool { return s.Queued == 1 })
	f.router.stop()
	if result := awaitFamilyAcquire(t, queued); statusCodeFromError(result.err) != http.StatusServiceUnavailable {
		t.Fatalf("stop left a waiter: %v", result.err)
	}
	active.release()
	reopened := newFamilyRouter(f.router.cfg)
	defer reopened.stop()
	if reopened.stateErr != nil {
		t.Fatal(reopened.stateErr)
	}
}
