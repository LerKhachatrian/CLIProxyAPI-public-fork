package logging

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestResponseObservationCaptureTimeAndFrameIsolation(t *testing.T) {
	ctx := WithFreshResponseHeadersHolder(context.Background())
	before := time.Now()
	SetResponseHeaders(ctx, http.Header{"X-Codex-Primary-Used-Percent": {"15"}, "Content-Type": {"application/json"}})
	_, at := GetResponseObservation(ctx)
	if at.Before(before) || at.After(time.Now()) {
		t.Fatal("capture timestamp missing")
	}
	// Simulate a long stream without a real sleep: completion does not change the
	// recorded watermark, while the next actual frame gets its own snapshot.
	holder := ctx.Value(responseHeadersKey{}).(*responseHeadersHolder)
	holder.observedAt = at.Add(-time.Hour)
	_, oldAt := GetResponseObservation(ctx)
	if !oldAt.Equal(at.Add(-time.Hour)) {
		t.Fatal("read advanced freshness")
	}
	MergeResponseHeaders(ctx, http.Header{"Retry-After": {"900"}})
	observation, newAt := GetResponseObservation(ctx)
	if observation.Get("X-Codex-Primary-Used-Percent") != "" || observation.Get("Retry-After") != "900" || !newAt.After(oldAt) {
		t.Fatal("partial frame inherited old window")
	}
	if GetResponseHeaders(ctx).Get("Content-Type") == "" {
		t.Fatal("diagnostic header merge regressed")
	}
	observation.Set("Retry-After", "1")
	copy, _ := GetResponseObservation(ctx)
	if copy.Get("Retry-After") != "900" {
		t.Fatal("mutable observation escaped")
	}
	if headers, at := GetResponseObservation(WithFreshResponseHeadersHolder(ctx)); headers != nil || !at.IsZero() {
		t.Fatal("retry inherited prior attempt observation")
	}
}
