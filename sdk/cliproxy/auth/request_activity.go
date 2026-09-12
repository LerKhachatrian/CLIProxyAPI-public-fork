package auth

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"time"

	codexauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codex"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// RequestActivitySnapshot is runtime-only evidence, not quota, routing state or
// a request log. One cell is shared by clones of a registered Codex identity.
type RequestActivitySnapshot struct {
	InFlight      int
	LastCompleted time.Time
}

type requestActivity struct {
	mu sync.Mutex
	RequestActivitySnapshot
}

func (a *Auth) RequestActivitySnapshot() RequestActivitySnapshot {
	if a == nil || a.requestActivity == nil {
		return RequestActivitySnapshot{}
	}
	a.requestActivity.mu.Lock()
	defer a.requestActivity.mu.Unlock()
	return a.requestActivity.RequestActivitySnapshot
}

func (a *Auth) resetRequestActivity() {
	a.requestActivity = nil
	if strings.EqualFold(a.Provider, "codex") && !a.Disabled && a.Status != StatusDisabled {
		a.requestActivity = &requestActivity{}
	}
}

func sameRequestActivityIdentity(a, b *Auth) bool {
	if a == nil || b == nil || a.ID != b.ID || a.Index != b.Index || a.RegistrationEpoch != b.RegistrationEpoch ||
		!strings.EqualFold(a.Provider, b.Provider) || a.Disabled || b.Disabled || a.Status == StatusDisabled || b.Status == StatusDisabled {
		return false
	}
	// Compare identity metadata in memory only. A normal token/priority refresh
	// is not an account replacement; changed identity retires the old cell.
	for _, field := range []string{"account_id", "email", "plan_type"} {
		if !reflect.DeepEqual(a.Metadata[field], b.Metadata[field]) {
			return false
		}
	}
	for _, field := range []string{"email", "account_email"} {
		if a.Attributes[field] != b.Attributes[field] {
			return false
		}
	}
	if reflect.DeepEqual(a.Metadata["id_token"], b.Metadata["id_token"]) {
		return true
	}
	// Inspect only a changed ID token at the auth update boundary, never on an
	// ordinary attempt or snapshot. Reuse the provider's parser, bound input and
	// compare stable identity claims, not refresh timestamps or signatures. This
	// is display-activity invalidation, not credential verification/authorization.
	identity := func(a *Auth) ([4]string, bool) {
		raw, ok := a.Metadata["id_token"].(string)
		if !ok || len(raw) > 64*1024 {
			return [4]string{}, false
		}
		claims, err := codexauth.ParseJWTToken(strings.TrimSpace(raw))
		if err != nil || claims == nil {
			return [4]string{}, false
		}
		return [4]string{strings.TrimSpace(claims.CodexAuthInfo.ChatgptAccountID),
			strings.TrimSpace(claims.CodexAuthInfo.ChatgptPlanType),
			strings.TrimSpace(claims.Email), strings.TrimSpace(claims.Sub)}, true
	}
	left, leftOK := identity(a)
	right, rightOK := identity(b)
	return leftOK && rightOK && left == right
}

type requestActivityScope struct {
	mu         sync.Mutex
	cell       *requestActivity
	started    bool
	closed     bool
	stopCancel func() bool
}

// observeRequestActivity attaches to the existing upstream-attempt boundary.
// Preparing/selecting an account does not start activity. The scope is owned by
// the execution or its existing stream pump; cancellation is a second,
// idempotent retirement path, not a polling goroutine or a request timeout.
func observeRequestActivity(ctx context.Context, a *Auth) (context.Context, func()) {
	if a == nil || a.requestActivity == nil {
		return ctx, func() {}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	scope := &requestActivityScope{cell: a.requestActivity}
	scope.mu.Lock()
	scope.stopCancel = context.AfterFunc(ctx, scope.finish)
	scope.mu.Unlock()
	return cliproxyexecutor.WithUpstreamAttemptObserver(ctx, scope.start), scope.finish
}

func (s *requestActivityScope) start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.closed {
		return
	}
	s.started = true
	s.cell.mu.Lock()
	s.cell.InFlight++
	s.cell.mu.Unlock()
}

func (s *requestActivityScope) finish() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	if s.started {
		s.cell.mu.Lock()
		s.cell.InFlight--
		now := time.Now()
		if now.After(s.cell.LastCompleted) {
			s.cell.LastCompleted = now
		}
		s.cell.mu.Unlock()
	}
	stop := s.stopCancel
	s.mu.Unlock()
	if stop != nil {
		stop()
	}
}

func executeWithRequestActivity(ctx context.Context, executor ProviderExecutor, a *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	ctx, release, errAdmission := admitFamilyRequest(ctx, a, opts)
	if errAdmission != nil {
		return cliproxyexecutor.Response{}, errAdmission
	}
	defer release()
	ctx, finish := observeRequestActivity(ctx, a)
	defer finish()
	if errGuard := cliproxyexecutor.CheckUpstreamAttempt(ctx); errGuard != nil {
		return cliproxyexecutor.Response{}, errGuard
	}
	return executor.Execute(ctx, a, req, opts)
}
