package executor

import (
	"context"
	"errors"
	"sync/atomic"
)

type downstreamWebsocketContextKey struct{}
type requireUpstreamWebsocketContextKey struct{}
type upstreamAttemptTrackerContextKey struct{}
type upstreamAttemptObserverContextKey struct{}
type upstreamAttemptGuardContextKey struct{}

// WithUpstreamAttemptGuard installs a bounded memory-only admission check.
// It runs immediately before each transport attempt, including reconnects.
// Unlike the observer, it may refuse an attempt before any provider bytes.
func WithUpstreamAttemptGuard(ctx context.Context, guard func() error) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, upstreamAttemptGuardContextKey{}, guard)
}

type upstreamAttemptGuardError struct{ cause error }

func (e *upstreamAttemptGuardError) Error() string { return e.cause.Error() }
func (e *upstreamAttemptGuardError) Unwrap() error { return e.cause }

func IsUpstreamAttemptGuardError(err error) bool {
	var guardErr *upstreamAttemptGuardError
	return errors.As(err, &guardErr)
}

func CheckUpstreamAttempt(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if guard, ok := ctx.Value(upstreamAttemptGuardContextKey{}).(func() error); ok && guard != nil {
		if err := guard(); err != nil {
			return &upstreamAttemptGuardError{cause: err}
		}
	}
	return nil
}

type upstreamAttemptTracker struct {
	attempted atomic.Bool
}

// WithDownstreamWebsocket marks the current request as coming from a downstream websocket connection.
func WithDownstreamWebsocket(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, downstreamWebsocketContextKey{}, true)
}

// DownstreamWebsocket reports whether the current request originates from a downstream websocket connection.
func DownstreamWebsocket(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	raw := ctx.Value(downstreamWebsocketContextKey{})
	enabled, ok := raw.(bool)
	return ok && enabled
}

// WithRequiredUpstreamWebsocket marks a request whose incremental context is valid only on the current upstream websocket.
func WithRequiredUpstreamWebsocket(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, requireUpstreamWebsocketContextKey{}, true)
}

// RequiredUpstreamWebsocket reports whether falling back to an HTTP upstream would lose request context.
func RequiredUpstreamWebsocket(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	raw := ctx.Value(requireUpstreamWebsocketContextKey{})
	enabled, ok := raw.(bool)
	return ok && enabled
}

// WithUpstreamAttemptTracker installs a fresh tracker for one provider execution attempt.
func WithUpstreamAttemptTracker(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, upstreamAttemptTrackerContextKey{}, &upstreamAttemptTracker{})
}

// WithUpstreamAttemptObserver observes actual transport attempts inside an
// execution lifetime. Fresh retry trackers inherit it; the owner must make its
// callback bounded, nonblocking and idempotent. No payload or credential crosses
// this boundary, and the observer cannot affect attempt/routing decisions.
func WithUpstreamAttemptObserver(ctx context.Context, observer func()) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, upstreamAttemptObserverContextKey{}, observer)
}

// MarkUpstreamAttempt records that the provider execution reached an upstream transport boundary.
func MarkUpstreamAttempt(ctx context.Context) {
	if ctx == nil {
		return
	}
	tracker, ok := ctx.Value(upstreamAttemptTrackerContextKey{}).(*upstreamAttemptTracker)
	if !ok || tracker == nil {
		return
	}
	if !tracker.attempted.Swap(true) {
		if observer, ok := ctx.Value(upstreamAttemptObserverContextKey{}).(func()); ok && observer != nil {
			observer()
		}
	}
}

// UpstreamAttempted reports whether the tracked provider execution reached an upstream transport boundary.
func UpstreamAttempted(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	tracker, ok := ctx.Value(upstreamAttemptTrackerContextKey{}).(*upstreamAttemptTracker)
	return ok && tracker != nil && tracker.attempted.Load()
}
