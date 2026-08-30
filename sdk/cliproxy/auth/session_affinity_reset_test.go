package auth

import (
	"context"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestSessionCacheClearReturnsKeyCountAndIsIdempotent(t *testing.T) {
	cache := NewSessionCache(time.Hour)
	defer cache.Stop()

	cache.SetAliases("auth-a", "primary", "fallback")
	if got := cache.Len(); got != 2 {
		t.Fatalf("Len() = %d, want 2", got)
	}
	if got := cache.Clear(); got != 2 {
		t.Fatalf("Clear() = %d, want 2", got)
	}
	if got := cache.Len(); got != 0 {
		t.Fatalf("Len() after Clear = %d, want 0", got)
	}
	if got := cache.Clear(); got != 0 {
		t.Fatalf("second Clear() = %d, want 0", got)
	}
}

func TestSessionAffinityResetReevaluatesCurrentPriority(t *testing.T) {
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &RoundRobinSelector{},
		TTL:      time.Hour,
	})
	defer selector.Stop()

	authA := &Auth{ID: "auth-a", Provider: "codex", Status: StatusActive, Attributes: map[string]string{"priority": "20"}}
	authB := &Auth{ID: "auth-b", Provider: "codex", Status: StatusActive, Attributes: map[string]string{"priority": "10"}}
	auths := []*Auth{authA, authB}
	opts := cliproxyexecutor.Options{Metadata: map[string]any{
		cliproxyexecutor.DerivedSessionIDMetadataKey: "priority-reset-session",
	}}

	pick := func() *Auth {
		t.Helper()
		auth, errPick := selector.Pick(context.Background(), "codex", "gpt-test", opts, auths)
		if errPick != nil {
			t.Fatalf("Pick(): %v", errPick)
		}
		if auth == nil {
			t.Fatal("Pick() returned nil auth")
		}
		return auth
	}

	if got := pick().ID; got != authA.ID {
		t.Fatalf("cold Pick() = %q, want %q", got, authA.ID)
	}
	authA.Attributes["priority"] = "10"
	authB.Attributes["priority"] = "20"
	if got := pick().ID; got != authA.ID {
		t.Fatalf("sticky Pick() = %q, want existing binding %q", got, authA.ID)
	}

	if cleared := selector.ResetSessionAffinity(); cleared != 1 {
		t.Fatalf("ResetSessionAffinity() = %d, want 1", cleared)
	}
	if got := pick().ID; got != authB.ID {
		t.Fatalf("Pick() after reset = %q, want new high priority %q", got, authB.ID)
	}
}

type blockingAffinityFallback struct {
	entered chan struct{}
	release chan struct{}
}

func (s *blockingAffinityFallback) Pick(_ context.Context, _, _ string, _ cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	close(s.entered)
	<-s.release
	return auths[0], nil
}

func TestSessionAffinityResetLinearizesWithBlockedPick(t *testing.T) {
	fallback := &blockingAffinityFallback{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: fallback,
		TTL:      time.Hour,
	})
	defer selector.Stop()

	pickDone := make(chan error, 1)
	go func() {
		_, errPick := selector.Pick(
			context.Background(),
			"codex",
			"gpt-test",
			cliproxyexecutor.Options{Metadata: map[string]any{
				cliproxyexecutor.DerivedSessionIDMetadataKey: "blocked-pick-session",
			}},
			[]*Auth{{ID: "auth-a", Provider: "codex", Status: StatusActive}},
		)
		pickDone <- errPick
	}()

	select {
	case <-fallback.entered:
	case <-time.After(time.Second):
		t.Fatal("Pick() did not reach blocking fallback")
	}

	resetDone := make(chan int, 1)
	go func() {
		resetDone <- selector.ResetSessionAffinity()
	}()

	select {
	case cleared := <-resetDone:
		t.Fatalf("reset returned before blocked Pick() finished; cleared %d", cleared)
	case <-time.After(50 * time.Millisecond):
	}

	close(fallback.release)
	select {
	case errPick := <-pickDone:
		if errPick != nil {
			t.Fatalf("Pick(): %v", errPick)
		}
	case <-time.After(time.Second):
		t.Fatal("Pick() did not finish")
	}

	select {
	case cleared := <-resetDone:
		if cleared != 1 {
			t.Fatalf("ResetSessionAffinity() = %d, want 1", cleared)
		}
	case <-time.After(time.Second):
		t.Fatal("ResetSessionAffinity() did not finish")
	}
	if got := selector.SessionAffinityBindingCount(); got != 0 {
		t.Fatalf("binding count after reset = %d, want 0", got)
	}
}

func TestSessionAffinityDelayedResultCannotRecreateResetBinding(t *testing.T) {
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &RoundRobinSelector{},
		TTL:      time.Hour,
	})
	defer selector.Stop()

	opts := cliproxyexecutor.Options{Metadata: map[string]any{
		cliproxyexecutor.DerivedSessionIDMetadataKey: "delayed-result-session",
	}}
	auth, errPick := selector.Pick(
		context.Background(),
		"codex",
		"gpt-test",
		opts,
		[]*Auth{{ID: "auth-a", Provider: "codex", Status: StatusActive}},
	)
	if errPick != nil {
		t.Fatalf("Pick(): %v", errPick)
	}
	if cleared := selector.ResetSessionAffinity(); cleared != 1 {
		t.Fatalf("ResetSessionAffinity() = %d, want 1", cleared)
	}

	selector.OnResult(Result{
		AuthID:   auth.ID,
		Provider: "codex",
		Model:    "gpt-test",
		Success:  true,
		Options:  opts,
	})
	if got := selector.SessionAffinityBindingCount(); got != 0 {
		t.Fatalf("delayed OnResult recreated %d binding(s)", got)
	}
}

func TestManagerSessionAffinityStatusAndReset(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	if count, enabled := manager.SessionAffinityStatus(); count != 0 || enabled {
		t.Fatalf("default status = (%d, %t), want (0, false)", count, enabled)
	}
	if cleared, enabled := manager.ResetSessionAffinity(); cleared != 0 || enabled {
		t.Fatalf("default reset = (%d, %t), want (0, false)", cleared, enabled)
	}

	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &RoundRobinSelector{},
		TTL:      time.Hour,
	})
	defer selector.Stop()
	manager.SetSelector(selector)
	selector.cache.Set("session-key", "auth-a")

	if count, enabled := manager.SessionAffinityStatus(); count != 1 || !enabled {
		t.Fatalf("affinity status = (%d, %t), want (1, true)", count, enabled)
	}
	if cleared, enabled := manager.ResetSessionAffinity(); cleared != 1 || !enabled {
		t.Fatalf("affinity reset = (%d, %t), want (1, true)", cleared, enabled)
	}
	if count, enabled := manager.SessionAffinityStatus(); count != 0 || !enabled {
		t.Fatalf("status after reset = (%d, %t), want (0, true)", count, enabled)
	}
}
