package auth

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func newFamilyManager(t *testing.T, accounts, exhausted int) (*Manager, *familyRouter, []*Auth) {
	t.Helper()
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Family: &FamilyRoutingConfig{StatePath: filepath.Join(t.TempDir(), "families.json")}})
	manager := NewManager(nil, selector, nil)
	t.Cleanup(func() { manager.SetSelector(nil) })
	manager.SetRetryConfig(0, 0, accounts)
	manager.SetFamilyQuotaSource(func(a *Auth) FamilyQuotaObservation {
		used := float64(10)
		if a.Attributes["synthetic_exhausted"] == "true" {
			used = 100
		}
		return FamilyQuotaObservation{Available: true, Identity: a.CodexAccountIdentity(), RegistrationEpoch: a.RegistrationEpoch,
			ObservedAt: a.CreatedAt, WeeklyKnown: true, WeeklyUsedPercent: used, WeeklyResetAt: a.CreatedAt.Add(7 * 24 * time.Hour)}
	})
	reg := registry.GetGlobalRegistry()
	auths := make([]*Auth, 0, accounts)
	for i := 0; i < accounts; i++ {
		id := fmt.Sprintf("%s-%02d", t.Name(), i)
		a := &Auth{ID: id, Index: fmt.Sprintf("family-index-%02d", i), indexAssigned: true, Provider: "codex", Status: StatusActive,
			Attributes: map[string]string{"priority": fmt.Sprint((accounts - i) * 100), "synthetic_exhausted": fmt.Sprint(i < exhausted)}}
		registered, err := manager.Register(t.Context(), a)
		if err != nil {
			t.Fatal(err)
		}
		auths = append(auths, registered)
		reg.RegisterClient(id, "codex", []*registry.ModelInfo{{ID: "family-model-a"}, {ID: "family-model-b"}, {ID: "family-upstream"}})
		t.Cleanup(func() { reg.UnregisterClient(id) })
	}
	return manager, selector.family, auths
}

func TestFamilyManagerExecuteAndCountNeverAttemptWeeklyExhaustedAccounts(t *testing.T) {
	manager, _, auths := newFamilyManager(t, 10, 6)
	counts := map[string]int{}
	manager.RegisterExecutor(&mockCustomErrorExecutor{identifier: "codex",
		executeFn: func(ctx context.Context, a *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
			cliproxyexecutor.MarkUpstreamAttempt(ctx)
			counts[a.ID]++
			return cliproxyexecutor.Response{Payload: []byte(a.ID)}, nil
		},
		countFn: func(ctx context.Context, a *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
			cliproxyexecutor.MarkUpstreamAttempt(ctx)
			return cliproxyexecutor.Response{Payload: []byte(a.ID)}, nil
		}})
	for i := 0; i < 20; i++ {
		root := fmt.Sprintf("root-%02d", i)
		resp, err := manager.Execute(t.Context(), []string{"codex"}, cliproxyexecutor.Request{Model: "family-model-a"}, familyOptions(root, root, ""))
		if err != nil {
			t.Fatal(err)
		}
		opts := familyOptions(root, root+"-child", root)
		opts.OriginalRequest = []byte(`{"service_tier":"priority"}`)
		count, err := manager.ExecuteCount(t.Context(), []string{"codex"}, cliproxyexecutor.Request{Model: "family-model-b"}, opts)
		if err != nil || string(count.Payload) != string(resp.Payload) {
			t.Fatalf("mixed-model/tier count escaped family: %v", err)
		}
	}
	for i, a := range auths {
		want := 5
		if i < 6 {
			want = 0
		}
		if counts[a.ID] != want {
			t.Fatalf("account %d got %d provider calls, want %d", i, counts[a.ID], want)
		}
	}
}

func TestFamilyManagerPinnedContinuationRequiresReplayBeforeExecutor(t *testing.T) {
	for _, mode := range []string{"reset", "disabled", "weekly exhausted"} {
		t.Run(mode, func(t *testing.T) {
			manager, r, _ := newFamilyManager(t, 2, 0)
			calls := 0
			manager.RegisterExecutor(&mockCustomErrorExecutor{identifier: "codex", executeFn: func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
				calls++
				return cliproxyexecutor.Response{}, nil
			}})
			a, err := manager.SelectAuth(t.Context(), "codex", "family-model-a", familyOptions("root", "root", ""))
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "reset":
				r.resetAssignments()
			case "disabled":
				a.Disabled = true
				_, err = manager.Update(t.Context(), a)
			case "weekly exhausted":
				a.Attributes["synthetic_exhausted"] = "true"
				_, err = manager.Update(t.Context(), a)
			}
			if err != nil {
				t.Fatal(err)
			}
			opts := familyOptions("root", "root", "")
			opts.Metadata[cliproxyexecutor.PinnedAuthMetadataKey] = a.ID
			_, err = manager.Execute(t.Context(), []string{"codex"}, cliproxyexecutor.Request{Model: "family-model-a"}, opts)
			if !IsFamilyReselectError(err) || calls != 0 {
				t.Fatalf("pin failure attempted incomplete continuation: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestFamilyManagerAliasCooldownUsesUpstreamModel(t *testing.T) {
	manager, r, auths := newFamilyManager(t, 1, 0)
	manager.RegisterExecutor(&mockCustomErrorExecutor{identifier: "codex"})
	manager.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{"codex": {{Name: "family-upstream", Alias: "family-model-a", Fork: true}}})
	a := auths[0].Clone()
	a.ModelStates = map[string]*ModelState{"family-upstream": {Status: StatusError, Unavailable: true, NextRetryAfter: time.Now().Add(time.Hour)}}
	if _, err := manager.Update(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SelectAuth(t.Context(), "codex", "family-model-a", familyOptions("root", "root", "")); statusCodeFromError(err) != http.StatusServiceUnavailable {
		t.Fatalf("aliased cooldown was bypassed: %v", err)
	}
	// A cooldown on the alias itself must not block a healthy resolved model.
	a, _ = manager.GetByID(a.ID)
	a.ModelStates = map[string]*ModelState{"family-model-a": {Status: StatusError, Unavailable: true, NextRetryAfter: time.Now().Add(time.Hour)}, "family-upstream": {Status: StatusActive}}
	if _, err := manager.Update(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	opts := familyOptions("root", "root", "")
	if _, err := manager.SelectAuth(t.Context(), "codex", "family-model-a", opts); err != nil {
		t.Fatal(err)
	}
	lease, err := r.acquire(t.Context(), familyTokenFromOptions(opts))
	if err != nil {
		t.Fatal(err)
	}
	lease.release()
}

func TestFamilyManagerStreamFailoverOnlyBeforeOutput(t *testing.T) {
	for _, emitted := range []bool{false, true} {
		t.Run(fmt.Sprintf("emitted=%t", emitted), func(t *testing.T) {
			manager, r, _ := newFamilyManager(t, 2, 0)
			var mu sync.Mutex
			var attempts []string
			manager.RegisterExecutor(&customStreamMockExecutor{identifier: "codex", streamFn: func(ctx context.Context, a *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
				cliproxyexecutor.MarkUpstreamAttempt(ctx)
				mu.Lock()
				attempts = append(attempts, a.ID)
				first := len(attempts) == 1
				mu.Unlock()
				if first {
					err := customStatusError{code: http.StatusTooManyRequests, msg: "synthetic model capacity"}
					if !emitted {
						return nil, err
					}
					chunks := make(chan cliproxyexecutor.StreamChunk, 2)
					chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(`data: {"type":"response.output_text.delta","delta":"committed"}`)}
					chunks <- cliproxyexecutor.StreamChunk{Err: err}
					close(chunks)
					return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
				}
				return successStreamResult(), nil
			}})
			stream, err := manager.ExecuteStream(t.Context(), []string{"codex"}, cliproxyexecutor.Request{Model: "family-model-a"}, familyOptions("root", "root", ""))
			if err != nil {
				t.Fatal(err)
			}
			var sawError bool
			for chunk := range stream.Chunks {
				sawError = sawError || chunk.Err != nil
			}
			mu.Lock()
			actual := append([]string(nil), attempts...)
			mu.Unlock()
			if emitted {
				if len(actual) != 1 || !sawError {
					t.Fatalf("committed stream replayed: %v, error=%t", actual, sawError)
				}
			} else if len(actual) != 2 || actual[0] == actual[1] || sawError {
				t.Fatalf("pre-output failover: %v, error=%t", actual, sawError)
			}
			waitFamilyState(t, r, func(s FamilyRoutingStatus) bool { return s.InFlight == 0 && s.Queued == 0 })
			if r.status().WeeklyExcludedAccounts != 0 {
				t.Fatal("capacity 429 fabricated weekly exhaustion")
			}
		})
	}
}
