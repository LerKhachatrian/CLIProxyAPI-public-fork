package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type activityTestExecutor struct {
	quotaAttemptIsolationExecutor
	execute func(context.Context) (cliproxyexecutor.Response, error)
	stream  func(context.Context) (*cliproxyexecutor.StreamResult, error)
	prepare func(context.Context, *Auth) (*Auth, error)
}

func (e *activityTestExecutor) ShouldPrepareRequestAuth(*Auth) bool { return e.prepare != nil }
func (e *activityTestExecutor) PrepareRequestAuth(ctx context.Context, a *Auth) (*Auth, error) {
	return e.prepare(ctx, a)
}
func (e *activityTestExecutor) Execute(ctx context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return e.execute(ctx)
}
func (e *activityTestExecutor) ExecuteStream(ctx context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return e.stream(ctx)
}
func (e *activityTestExecutor) CountTokens(ctx context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	cliproxyexecutor.MarkUpstreamAttempt(ctx)
	return cliproxyexecutor.Response{}, nil
}

func activityTestAuth(t *testing.T, m *Manager, id string) *Auth {
	t.Helper()
	a, err := m.Register(context.Background(), &Auth{ID: id, Provider: "codex", Status: StatusActive,
		Metadata: map[string]any{"email": id + "@example.invalid", "account_id": id, "plan_type": "pro"}})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func activityTestManager(t *testing.T, executor *activityTestExecutor) (*Manager, *Auth, cliproxyexecutor.Request) {
	t.Helper()
	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(0, 0, 1)
	m.RegisterExecutor(executor)
	a := activityTestAuth(t, m, "activity-"+t.Name())
	model := "synthetic-activity-model-" + t.Name()
	registry.GetGlobalRegistry().RegisterClient(a.ID, "codex", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
	return m, a, cliproxyexecutor.Request{Model: model}
}

func requireActivityClosed(t *testing.T, a *Auth) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for a.RequestActivitySnapshot().InFlight != 0 && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if a.RequestActivitySnapshot().InFlight != 0 {
		t.Fatal("activity did not retire")
	}
}

func TestRequestActivityScopeLifecycleAndCloneIsolation(t *testing.T) {
	m := NewManager(nil, nil, nil)
	a := activityTestAuth(t, m, "synthetic-activity")
	ctx, finish := observeRequestActivity(newUpstreamAttemptContext(context.Background()), a)
	defer finish()
	if got := a.RequestActivitySnapshot(); got.InFlight != 0 || !got.LastCompleted.IsZero() {
		t.Fatal("scope creation became actual activity")
	}
	cliproxyexecutor.MarkUpstreamAttempt(ctx)
	cliproxyexecutor.MarkUpstreamAttempt(newUpstreamAttemptContext(ctx))
	clone := a.Clone()
	if got := clone.RequestActivitySnapshot(); got.InFlight != 1 || !got.LastCompleted.IsZero() {
		t.Fatal("clones or retry marks duplicated the scope")
	}
	encoded, err := json.Marshal(a)
	if err != nil || strings.Contains(string(encoded), "InFlight") || strings.Contains(string(encoded), "requestActivity") || strings.Contains(string(encoded), "LastCompleted") {
		t.Fatal("activity leaked into persistence")
	}
	finish()
	completed := clone.RequestActivitySnapshot().LastCompleted
	finish()
	cliproxyexecutor.MarkUpstreamAttempt(newUpstreamAttemptContext(ctx))
	if got := clone.RequestActivitySnapshot(); got.InFlight != 0 || completed.IsZero() || !got.LastCompleted.Equal(completed) {
		t.Fatal("duplicate/late cleanup revived or rejuvenated activity")
	}
	unusedCtx, unusedFinish := observeRequestActivity(newUpstreamAttemptContext(context.Background()), a)
	_ = unusedCtx
	unusedFinish()
	if !a.RequestActivitySnapshot().LastCompleted.Equal(completed) {
		t.Fatal("unattempted scope changed recent activity")
	}
}

func TestRequestActivityPreservesMetadataRefreshButRetiresReplacement(t *testing.T) {
	for _, mutation := range []string{"priority", "token", "account", "email", "plan", "disabled", "reregister", "removed"} {
		t.Run(mutation, func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			a := activityTestAuth(t, m, "activity-replacement")
			ctx, finish := observeRequestActivity(newUpstreamAttemptContext(context.Background()), a)
			defer finish()
			cliproxyexecutor.MarkUpstreamAttempt(ctx)
			updated := a.Clone()
			switch mutation {
			case "priority":
				updated.Attributes = map[string]string{"priority": "2000"}
			case "token":
				updated.Metadata["access_token"] = "synthetic-rotated-token"
			case "account":
				updated.Metadata["account_id"] = "different-synthetic-account"
			case "email":
				updated.Metadata["email"] = "replacement@example.invalid"
			case "plan":
				updated.Metadata["plan_type"] = "plus"
			case "disabled":
				updated.Disabled = true
			case "removed":
				m.Remove(context.Background(), a.ID)
			}
			var err error
			if mutation == "reregister" || mutation == "removed" {
				_, err = m.Register(context.Background(), updated)
			} else {
				_, err = m.Update(context.Background(), updated)
			}
			if err != nil {
				t.Fatal(err)
			}
			current, _ := m.GetByID(a.ID)
			preserved := mutation == "priority" || mutation == "token"
			if (current.RequestActivitySnapshot().InFlight == 1) != preserved {
				t.Fatal("incorrect identity/activity preservation")
			}
			finish()
			if got := current.RequestActivitySnapshot(); got.InFlight != 0 || (!got.LastCompleted.IsZero()) != preserved {
				t.Fatal("retired scope modified a replacement identity")
			}
		})
	}
}

func TestRequestActivityConcurrentAccountsAndCancellation(t *testing.T) {
	m := NewManager(nil, nil, nil)
	accounts := make([]*Auth, 128)
	var finishers []func()
	var workers sync.WaitGroup
	t.Cleanup(func() {
		for _, finish := range finishers {
			finish()
		}
		workers.Wait()
	})
	for i := range accounts {
		accounts[i] = activityTestAuth(t, m, fmt.Sprintf("activity-%03d", i))
		for j := 0; j < 4; j++ {
			ctx, cancel := context.WithCancel(context.Background())
			ctx, finish := observeRequestActivity(newUpstreamAttemptContext(ctx), accounts[i])
			cliproxyexecutor.MarkUpstreamAttempt(ctx)
			finishers = append(finishers, func() { cancel(); finish(); finish() })
		}
	}
	for _, a := range accounts {
		if a.RequestActivitySnapshot().InFlight != 4 {
			t.Fatal("concurrent per-account activity was collapsed")
		}
	}
	for _, finish := range finishers {
		workers.Go(finish)
	}
	workers.Wait()
	for _, a := range accounts {
		if got := a.RequestActivitySnapshot(); got.InFlight != 0 || got.LastCompleted.IsZero() {
			t.Fatal("concurrent retirement leaked activity")
		}
	}
}

func TestRequestActivityRealExecuteBoundary(t *testing.T) {
	for _, attempted := range []bool{false, true} {
		t.Run(fmt.Sprint(attempted), func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			executor := &activityTestExecutor{execute: func(ctx context.Context) (cliproxyexecutor.Response, error) {
				if attempted {
					cliproxyexecutor.MarkUpstreamAttempt(ctx)
				}
				close(started)
				select {
				case <-release:
				case <-ctx.Done():
					return cliproxyexecutor.Response{}, ctx.Err()
				}
				return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
			}}
			m, a, req := activityTestManager(t, executor)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			var worker sync.WaitGroup
			worker.Go(func() { _, err := m.Execute(ctx, []string{"codex"}, req, cliproxyexecutor.Options{}); done <- err })
			t.Cleanup(func() { cancel(); worker.Wait() })
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("synthetic execution did not start")
			}
			if got := a.RequestActivitySnapshot(); (got.InFlight == 1) != attempted || !got.LastCompleted.IsZero() {
				t.Fatal("actual transport boundary was not respected")
			}
			close(release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if got := a.RequestActivitySnapshot(); got.InFlight != 0 || (!got.LastCompleted.IsZero()) != attempted {
				t.Fatal("completion activity mismatch")
			}
		})
	}
}

func TestRequestActivityStreamLifetimeAndCancellation(t *testing.T) {
	for _, cancelStream := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelStream), func(t *testing.T) {
			chunks := make(chan cliproxyexecutor.StreamChunk, 1)
			closeChunks := sync.OnceFunc(func() { close(chunks) })
			defer closeChunks()
			chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(`data: {"type":"response.output_text.delta","delta":"synthetic"}`)}
			executor := &activityTestExecutor{stream: func(ctx context.Context) (*cliproxyexecutor.StreamResult, error) {
				cliproxyexecutor.MarkUpstreamAttempt(ctx)
				return &cliproxyexecutor.StreamResult{Chunks: chunks, Headers: http.Header{}}, nil
			}}
			m, a, req := activityTestManager(t, executor)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stream, err := m.ExecuteStream(ctx, []string{"codex"}, req, cliproxyexecutor.Options{Stream: true})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				cancel()
				closeChunks()
				for range stream.Chunks {
				}
			})
			if a.RequestActivitySnapshot().InFlight != 1 {
				t.Fatal("returning stream headers retired a live request")
			}
			<-stream.Chunks
			if cancelStream {
				cancel()
				requireActivityClosed(t, a)
			}
			closeChunks()
			for range stream.Chunks {
			}
			requireActivityClosed(t, a)
			if a.RequestActivitySnapshot().LastCompleted.IsZero() {
				t.Fatal("stream retirement lost actual activity")
			}
		})
	}
}

func TestRequestActivityErrorsAndTokenCounting(t *testing.T) {
	executor := &activityTestExecutor{
		execute: func(ctx context.Context) (cliproxyexecutor.Response, error) {
			cliproxyexecutor.MarkUpstreamAttempt(ctx)
			return cliproxyexecutor.Response{}, errors.New("synthetic upstream error")
		},
		stream: func(ctx context.Context) (*cliproxyexecutor.StreamResult, error) {
			cliproxyexecutor.MarkUpstreamAttempt(ctx)
			return nil, errors.New("synthetic stream error")
		},
	}
	m, a, req := activityTestManager(t, executor)
	if _, err := m.ExecuteCount(context.Background(), []string{"codex"}, req, cliproxyexecutor.Options{}); err != nil {
		t.Fatal(err)
	}
	if got := a.RequestActivitySnapshot(); got.InFlight != 0 || !got.LastCompleted.IsZero() {
		t.Fatal("token counting became generation activity")
	}
	if _, err := executeWithRequestActivity(newUpstreamAttemptContext(context.Background()), executor, a, req, cliproxyexecutor.Options{}); err == nil {
		t.Fatal("missing synthetic error")
	}
	if got := a.RequestActivitySnapshot(); got.InFlight != 0 || got.LastCompleted.IsZero() {
		t.Fatal("failed execution leaked activity")
	}
	if _, err := m.executeStreamWithModelPool(context.Background(), executor, a, "codex", req, cliproxyexecutor.Options{}, req.Model, "", []string{req.Model}, false, OAuthModelAliasResult{}, nil, false, false); err == nil {
		t.Fatal("missing synthetic stream error")
	}
	requireActivityClosed(t, a)
}

func TestRequestActivityChangedClaimsAndAttributeIdentity(t *testing.T) {
	makeToken := func(account, plan, email, subject string, issued int) string {
		data, err := json.Marshal(map[string]any{"email": email, "sub": subject, "iat": issued,
			"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": account, "chatgpt_plan_type": plan}})
		if err != nil {
			t.Fatal(err)
		}
		return "synthetic." + base64.RawURLEncoding.EncodeToString(data) + ".signature"
	}
	for _, change := range []string{"refresh", "claims_account", "claims_plan", "claims_email", "claims_subject", "invalid", "missing", "oversize", "attribute_email"} {
		t.Run(change, func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			a := activityTestAuth(t, m, "claims-activity")
			a.Metadata["id_token"] = makeToken("first", "pro", "first@example.invalid", "first", 1)
			a, _ = m.Update(context.Background(), a)
			ctx, finish := observeRequestActivity(newUpstreamAttemptContext(context.Background()), a)
			defer finish()
			cliproxyexecutor.MarkUpstreamAttempt(ctx)
			updated := a.Clone()
			updated.Metadata["id_token"] = makeToken("first", "pro", "first@example.invalid", "first", 2)
			switch change {
			case "claims_account":
				updated.Metadata["id_token"] = makeToken("second", "pro", "first@example.invalid", "first", 2)
			case "claims_plan":
				updated.Metadata["id_token"] = makeToken("first", "plus", "first@example.invalid", "first", 2)
			case "claims_email":
				updated.Metadata["id_token"] = makeToken("first", "pro", "second@example.invalid", "first", 2)
			case "claims_subject":
				updated.Metadata["id_token"] = makeToken("first", "pro", "first@example.invalid", "second", 2)
			case "invalid":
				updated.Metadata["id_token"] = "synthetic-invalid"
			case "missing":
				delete(updated.Metadata, "id_token")
			case "oversize":
				updated.Metadata["id_token"] = strings.Repeat("x", 64*1024+1)
			case "attribute_email":
				updated.Attributes = map[string]string{"account_email": "second@example.invalid"}
			}
			current, err := m.Update(context.Background(), updated)
			if err != nil || current == nil {
				t.Fatal("identity update failed", err)
			}
			preserved := change == "refresh"
			if (current.RequestActivitySnapshot().InFlight == 1) != preserved {
				t.Fatal("changed claims incorrectly retained or retired activity")
			}
			finish()
			if got := current.RequestActivitySnapshot(); got.InFlight != 0 || (!got.LastCompleted.IsZero()) != preserved {
				t.Fatal("old completion contaminated changed identity")
			}
		})
	}
}

func TestRequestActivityRefreshPrepareAndLoadBoundaries(t *testing.T) {
	for _, mode := range []string{"refresh", "prepare", "load", "status_disabled", "provider"} {
		t.Run(mode, func(t *testing.T) {
			store := &schedulerLoadStore{}
			m := NewManager(store, nil, nil)
			a := activityTestAuth(t, m, "activity-lifecycle")
			ctx, finish := observeRequestActivity(newUpstreamAttemptContext(context.Background()), a)
			defer finish()
			cliproxyexecutor.MarkUpstreamAttempt(ctx)
			updated := a.Clone()
			updated.Metadata["access_token"] = "synthetic-refreshed"
			var err error
			switch mode {
			case "refresh":
				_, err = m.UpdateRefreshedAuth(context.Background(), a, updated)
			case "prepare":
				_, err = m.UpdatePreparedAuth(context.Background(), a, updated)
			case "load":
				store.auths = []*Auth{updated}
				err = m.Load(context.Background())
			case "status_disabled":
				updated.Status = StatusDisabled
				_, err = m.Update(context.Background(), updated)
			case "provider":
				updated.Provider = "synthetic-other-provider"
				_, err = m.Update(context.Background(), updated)
			}
			if err != nil {
				t.Fatal(err)
			}
			current, _ := m.GetByID(a.ID)
			preserved := mode == "refresh" || mode == "prepare"
			if (current.RequestActivitySnapshot().InFlight == 1) != preserved {
				t.Fatal("lifecycle boundary retained the wrong activity cell")
			}
			finish()
			if got := current.RequestActivitySnapshot(); got.InFlight != 0 || (!got.LastCompleted.IsZero()) != preserved {
				t.Fatal("retired lifecycle completion changed current activity")
			}
		})
	}
}

func TestRequestActivityCancellationStartRace(t *testing.T) {
	a := &Auth{Provider: "codex"}
	a.resetRequestActivity()
	for i := 0; i < 256; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		ctx, finish := observeRequestActivity(newUpstreamAttemptContext(ctx), a)
		var workers sync.WaitGroup
		workers.Go(func() { cliproxyexecutor.MarkUpstreamAttempt(ctx) })
		workers.Go(cancel)
		workers.Go(finish)
		workers.Wait()
		finish()
		before := a.RequestActivitySnapshot()
		cliproxyexecutor.MarkUpstreamAttempt(newUpstreamAttemptContext(ctx))
		if got := a.RequestActivitySnapshot(); got.InFlight != 0 || got != before {
			t.Fatal("late start or concurrent cancellation revived a retired scope")
		}
	}
}

func TestRequestActivityPreparationFailureDoesNotStartGeneration(t *testing.T) {
	executor := &activityTestExecutor{prepare: func(ctx context.Context, a *Auth) (*Auth, error) {
		cliproxyexecutor.MarkUpstreamAttempt(ctx)
		return nil, errors.New("synthetic preparation failure")
	}, execute: func(context.Context) (cliproxyexecutor.Response, error) {
		t.Fatal("generation ran after failed preparation")
		return cliproxyexecutor.Response{}, nil
	}}
	m, a, req := activityTestManager(t, executor)
	if _, err := m.Execute(context.Background(), []string{"codex"}, req, cliproxyexecutor.Options{}); err == nil {
		t.Fatal("missing preparation failure")
	}
	if got := a.RequestActivitySnapshot(); got.InFlight != 0 || !got.LastCompleted.IsZero() {
		t.Fatal("preparation transport became generation activity")
	}
}

func TestRequestActivityStreamBootstrapFailureAndCancellation(t *testing.T) {
	for _, mode := range []string{"error", "empty", "cancel", "retry"} {
		t.Run(mode, func(t *testing.T) {
			chunks := make(chan cliproxyexecutor.StreamChunk, 1)
			closeChunks := sync.OnceFunc(func() { close(chunks) })
			defer closeChunks()
			started := make(chan struct{})
			attempts := 0
			executor := &activityTestExecutor{stream: func(ctx context.Context) (*cliproxyexecutor.StreamResult, error) {
				cliproxyexecutor.MarkUpstreamAttempt(ctx)
				attempts++
				if attempts == 1 {
					close(started)
					return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
				}
				tail := make(chan cliproxyexecutor.StreamChunk, 1)
				tail <- cliproxyexecutor.StreamChunk{Payload: []byte("synthetic retry")}
				close(tail)
				return &cliproxyexecutor.StreamResult{Chunks: tail}, nil
			}}
			m, a, req := activityTestManager(t, executor)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			var worker sync.WaitGroup
			worker.Go(func() {
				models := []string{req.Model}
				if mode == "retry" {
					models = append(models, req.Model+"-retry")
				}
				stream, err := m.executeStreamWithModelPool(ctx, executor, a, "codex", req, cliproxyexecutor.Options{}, req.Model, "", models, false, OAuthModelAliasResult{}, nil, false, false)
				if stream != nil {
					for range stream.Chunks {
					}
				}
				done <- err
			})
			t.Cleanup(func() { cancel(); closeChunks(); worker.Wait() })
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("bootstrap never started")
			}
			if got := a.RequestActivitySnapshot(); got.InFlight != 1 || !got.LastCompleted.IsZero() {
				t.Fatal("bootstrap waiting was not active")
			}
			switch mode {
			case "error", "retry":
				chunks <- cliproxyexecutor.StreamChunk{Err: errors.New("synthetic bootstrap failure")}
			case "empty":
				closeChunks()
			case "cancel":
				cancel()
			}
			if err := <-done; (err == nil) != (mode == "retry") {
				t.Fatal("unexpected bootstrap result", err)
			}
			requireActivityClosed(t, a)
			if a.RequestActivitySnapshot().LastCompleted.IsZero() {
				t.Fatal("failed/cancelled bootstrap lost activity")
			}
		})
	}
}

func BenchmarkRequestActivityLifecycle(b *testing.B) {
	a := &Auth{Provider: "codex"}
	a.resetRequestActivity()
	b.ReportAllocs()
	for b.Loop() {
		ctx, finish := observeRequestActivity(newUpstreamAttemptContext(context.Background()), a)
		cliproxyexecutor.MarkUpstreamAttempt(ctx)
		finish()
	}
}
