package management

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/api/handlers/management/codexmonitor"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type monitorRoundTripper func(*http.Request) (*http.Response, error)

type monitorActivityExecutor struct {
	coreauth.ProviderExecutor // Unused paths fail loudly instead of doing real I/O.
	started                   chan string
	release                   <-chan struct{}
}

func (*monitorActivityExecutor) Identifier() string { return "codex" }
func (e *monitorActivityExecutor) Execute(ctx context.Context, a *coreauth.Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	cliproxyexecutor.MarkUpstreamAttempt(ctx)
	e.started <- a.ID
	select {
	case <-e.release:
		return cliproxyexecutor.Response{Payload: []byte(`{"synthetic":true}`)}, nil
	case <-ctx.Done():
		return cliproxyexecutor.Response{}, ctx.Err()
	}
}

func TestMonitorActivityTracksEveryRoundRobinAccount(t *testing.T) {
	release := make(chan struct{})
	closeRelease := sync.OnceFunc(func() { close(release) })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	defer closeRelease()
	executor := &monitorActivityExecutor{started: make(chan string, 8), release: release}
	m := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	m.SetRetryConfig(0, 0, 1)
	m.RegisterExecutor(executor)
	h := &Handler{authManager: m, configFilePath: filepath.Join(t.TempDir(), "synthetic.yaml")}
	model := "synthetic-round-robin-activity"
	accounts := make([]*coreauth.Auth, 7)
	for i := range accounts {
		priority := "1000"
		if i == 4 || i == 5 {
			priority = fmt.Sprint(9000 - 100*i)
		} else if i == 6 {
			priority = "100"
		}
		a, err := m.Register(ctx, &coreauth.Auth{ID: fmt.Sprintf("synthetic-round-robin-%d", i), Provider: "codex", Status: coreauth.StatusActive,
			Attributes: map[string]string{"priority": priority}, Metadata: map[string]any{"account_id": fmt.Sprint(i), "plan_type": "pro"}})
		if err != nil {
			t.Fatal(err)
		}
		accounts[i] = a
		// Four equal-priority accounts serve this model. Two unused high-priority
		// accounts are likely-next for the pool; the last account is reserve.
		if i < 4 {
			registry.GetGlobalRegistry().RegisterClient(a.ID, "codex", []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
		}
	}
	var workers sync.WaitGroup
	done := make(chan error, 8)
	t.Cleanup(func() {
		cancel()
		closeRelease()
		workers.Wait()
		if err := h.CloseCodexMonitor(); err != nil {
			t.Error(err)
		}
	})
	selected := map[string]int{}
	for i := 0; i < 8; i++ {
		workers.Go(func() {
			_, err := m.Execute(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
			done <- err
		})
		select {
		case id := <-executor.started:
			selected[id]++
		case err := <-done:
			t.Fatal("execution returned before the held transport", err)
		case <-ctx.Done():
			t.Fatal("round-robin transport did not start")
		}
	}
	if len(selected) != 4 {
		t.Fatalf("round robin selected %d accounts, want 4", len(selected))
	}
	for _, a := range accounts[:4] {
		if selected[a.ID] != 2 || a.RequestActivitySnapshot().InFlight != 2 {
			t.Fatal("per-account concurrent activity collapsed")
		}
	}
	checkClasses := func() {
		t.Helper()
		monitor, ids, err := h.monitorInputs()
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := monitor.Snapshot(ids, codexmonitor.Policy{UsageSeconds: 1800})
		if err != nil {
			t.Fatal(err)
		}
		classes := map[string]int{}
		for _, account := range snapshot.Accounts {
			classes[account.Activity]++
		}
		if classes["active"] != 4 || classes["likely_next"] != 2 || classes["reserve"] != 1 {
			t.Fatalf("actual/likely-next/reserve classes = %v", classes)
		}
	}
	checkClasses()
	closeRelease()
	workers.Wait()
	for i := 0; i < 8; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	for _, a := range accounts[:4] {
		if got := a.RequestActivitySnapshot(); got.InFlight != 0 || got.LastCompleted.IsZero() {
			t.Fatal("completed round-robin activity was lost")
		}
	}
	checkClasses()
}

func TestMonitorActivityWindowIsExactAndDistinctFromPriority(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name     string
		activity coreauth.RequestActivitySnapshot
		want     bool
	}{
		{"unused", coreauth.RequestActivitySnapshot{}, false},
		{"inflight", coreauth.RequestActivitySnapshot{InFlight: 3}, true},
		{"recent", coreauth.RequestActivitySnapshot{LastCompleted: now.Add(-time.Hour + time.Nanosecond)}, true},
		{"expired", coreauth.RequestActivitySnapshot{LastCompleted: now.Add(-time.Hour)}, false},
		{"future", coreauth.RequestActivitySnapshot{LastCompleted: now.Add(time.Second)}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if recentMonitorActivity(test.activity, now) != test.want {
				t.Fatal("activity boundary mismatch")
			}
		})
	}
	h, a := monitorTestHandler(t)
	// Legacy completed/preparation records and high priority are not evidence of
	// a transport-backed in-flight/recent scope.
	h.authManager.MarkResult(context.Background(), coreauth.Result{AuthID: a.ID, Provider: "codex", Success: true})
	ids, err := h.monitorIdentities()
	if err != nil || len(ids) != 1 || !ids[0].Active || ids[0].InUse {
		t.Fatal("likely-next priority or legacy buckets became actual activity", err)
	}
}

func (f monitorRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func monitorTestHandler(t *testing.T) (*Handler, *coreauth.Auth) {
	t.Helper()
	m := coreauth.NewManager(nil, nil, nil)
	a := &coreauth.Auth{ID: "synthetic-monitor", Provider: "codex", Status: coreauth.StatusActive,
		Metadata:   map[string]any{"access_token": "synthetic-only-token", "account_id": "synthetic-account", "plan_type": "prolite"},
		Attributes: map[string]string{"priority": "1000"}}
	if _, err := m.Register(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	h := &Handler{authManager: m, configFilePath: filepath.Join(t.TempDir(), "synthetic.yaml")}
	t.Cleanup(func() {
		if err := h.CloseCodexMonitor(); err != nil {
			t.Error(err)
		}
	})
	return h, a
}

func TestCodexMonitorFixedTransportAndCapture(t *testing.T) {
	h, a := monitorTestHandler(t)
	ids, err := h.monitorIdentities()
	if err != nil || len(ids) != 1 {
		t.Fatal("synthetic identity", err)
	}
	for _, kind := range []string{codexmonitor.UsageLane, codexmonitor.ResetLane} {
		calls := 0
		h.codexMonitorTransport = func(selected *coreauth.Auth) http.RoundTripper {
			if selected.ID != a.ID {
				t.Fatal("wrong account")
			}
			return monitorRoundTripper(func(req *http.Request) (*http.Response, error) {
				calls++
				path := "usage"
				body := `{"plan_type":"prolite","rate_limit":{"secondary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_after_seconds":1000}}}`
				if kind == codexmonitor.ResetLane {
					path, body = "rate-limit-reset-credits", `{"available_count":0,"credits":[]}`
				}
				deadline, ok := req.Context().Deadline()
				if req.Method != "GET" || req.URL.String() != codexMonitorProviderBase+path || req.Body != nil ||
					req.Header.Get("Authorization") != "Bearer synthetic-only-token" || req.Header.Get("ChatGPT-Account-Id") != "synthetic-account" ||
					!ok || time.Until(deadline) > 8*time.Second || time.Until(deadline) < 7*time.Second {
					t.Fatal("fixed transport/deadline contract")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
			})
		}
		result := h.fetchMonitorObservation(context.Background(), ids[0], kind)
		if calls != 1 || result.Status != 200 || kind == codexmonitor.UsageLane && (result.Usage == nil || result.Usage.Plan != "pro_5x") ||
			kind == codexmonitor.ResetLane && (result.Bank == nil || !result.Bank.Complete) {
			t.Fatal("normalized observation missing")
		}
	}
}

type monitorErrorBody struct{ reads, closes int }

func (b *monitorErrorBody) Read([]byte) (int, error) { b.reads++; return 0, io.EOF }
func (b *monitorErrorBody) Close() error             { b.closes++; return nil }

func TestCodexMonitorRejectsRedirectErrorPagesAndOversize(t *testing.T) {
	h, _ := monitorTestHandler(t)
	ids, _ := h.monitorIdentities()
	for _, status := range []int{302, 401, 403, 429, 503, 200} {
		calls := 0
		body := &monitorErrorBody{}
		h.codexMonitorTransport = func(*coreauth.Auth) http.RoundTripper {
			return monitorRoundTripper(func(req *http.Request) (*http.Response, error) {
				calls++
				var content io.ReadCloser = body
				if status == 200 {
					content = io.NopCloser(strings.NewReader(strings.Repeat(" ", codexmonitor.MaxBodyBytes+1)))
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Location": {"https://unrelated.invalid/"}, "Retry-After": {"7200"}}, Body: content, Request: req}, nil
			})
		}
		result := h.fetchMonitorObservation(context.Background(), ids[0], codexmonitor.UsageLane)
		if calls != 1 || result.Usage != nil || status != 200 && (body.reads != 0 || body.closes != 1) || result.RetryAt.Before(time.Now().Add(119*time.Minute)) {
			t.Fatalf("unsafe redirect/body/retry handling for %d", status)
		}
	}
}

func TestCodexMonitorIdentityRaceAndExcludedAccount(t *testing.T) {
	h, a := monitorTestHandler(t)
	ids, _ := h.monitorIdentities()
	h.codexMonitorTransport = func(*coreauth.Auth) http.RoundTripper { t.Fatal("transport opened for invalid identity"); return nil }
	changed, _ := h.authManager.GetByID(a.ID)
	changed.Metadata["account_id"] = "replacement-synthetic-account"
	if _, err := h.authManager.Update(context.Background(), changed); err != nil {
		t.Fatal(err)
	}
	if result := h.fetchMonitorObservation(context.Background(), ids[0], codexmonitor.UsageLane); result.Usage != nil {
		t.Fatal("stale identity accepted")
	}
	ids, _ = h.monitorIdentities()
	changed.Unavailable, changed.NextRetryAfter = true, time.Now().Add(time.Hour)
	if _, err := h.authManager.Update(context.Background(), changed); err != nil {
		t.Fatal(err)
	}
	_ = h.fetchMonitorObservation(context.Background(), ids[0], codexmonitor.ResetLane)
	if monitorIdentity(changed, time.Now()).Blocked != true {
		t.Fatal("timed exclusion ignored")
	}
	changed.NextRetryAfter = time.Now().Add(-time.Minute)
	if monitorIdentity(changed, time.Now()).Blocked {
		t.Fatal("expired exclusion treated as indefinite")
	}
	changed.LastError = &coreauth.Error{HTTPStatus: 401}
	if !monitorIdentity(changed, time.Now()).Blocked {
		t.Fatal("deterministic rejection ignored")
	}
}

func TestCodexMonitorRequestValidationAndLocalOnlyGET(t *testing.T) {
	h, _ := monitorTestHandler(t)
	h.codexMonitorTransport = func(*coreauth.Auth) http.RoundTripper {
		t.Fatal("local/invalid request performed provider I/O")
		return nil
	}
	router := gin.New()
	router.GET("/monitor", h.GetCodexMonitor)
	router.POST("/monitor", h.StepCodexMonitor)
	for _, body := range []string{
		`{"refresh":"usage","refresh":"resets"}`, `{"policy":{"usage_seconds":0,"usage_seconds":1800}}`,
		`{"refresh":"consume"}`, `{"auth_index":"orphan"}`, `{"token":"not-accepted"}`, `{} {}`, `[]`,
	} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest("POST", "/monitor", strings.NewReader(body)))
		if recorder.Code != 400 {
			t.Fatalf("invalid request status %d", recorder.Code)
		}
	}
	for _, method := range []string{"GET", "POST"} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(method, "/monitor", strings.NewReader(`{}`)))
		if recorder.Code != 200 {
			t.Fatal("local snapshot rejected")
		}
		if strings.Contains(recorder.Body.String(), "synthetic-only-token") || strings.Contains(recorder.Body.String(), "synthetic-account") {
			t.Fatal("identity/credential leaked")
		}
	}
}

func TestCodexMonitorExplicitReadAndActionObservationBoundary(t *testing.T) {
	h, a := monitorTestHandler(t)
	prepare := func(method, url string) (func(*http.Response, []byte, time.Time), *http.Request) {
		req, _ := http.NewRequest(method, url, nil)
		req.Header.Set("Authorization", "Bearer synthetic-only-token")
		observer, err := h.prepareMonitorAPICall(a, req, true)
		if err != nil {
			t.Fatal(err)
		}
		return observer, req
	}
	for _, url := range []string{"https://unrelated.invalid/usage", codexMonitorProviderBase + "usage?account=other"} {
		observer, _ := prepare("GET", url)
		if observer != nil {
			t.Fatal("arbitrary URL adopted")
		}
	}
	observer, req := prepare("GET", codexMonitorProviderBase+"rate-limit-reset-credits")
	if observer == nil {
		t.Fatal("explicit bank read not observed")
	}
	at := time.Now().UTC().Add(-time.Second)
	observer(&http.Response{StatusCode: 200, Header: http.Header{}, Request: req}, []byte(`{"available_count":0,"credits":[]}`), at)
	m, ids, _ := h.monitorInputs()
	snapshot, err := m.Snapshot(ids, codexmonitor.Policy{})
	if err != nil || snapshot.Accounts[0].Bank == nil || !snapshot.Accounts[0].Bank.ObservedAt.Equal(at) {
		t.Fatal("explicit capture time lost")
	}
	// Successful and uncertain outcomes both leave old balances invalidated.
	finish, _ := prepare("POST", codexMonitorProviderBase+"rate-limit-reset-credits/consume")
	if finish == nil {
		t.Fatal("consume not invalidated before dispatch")
	}
	snapshot, _ = m.Snapshot(ids, codexmonitor.Policy{})
	if snapshot.Accounts[0].Bank != nil {
		t.Fatal("pre-action cache remains authoritative")
	}
	finish(nil, nil, time.Now())
	// An already in-flight old response cannot restore the bank.
	observer(&http.Response{StatusCode: 200, Header: http.Header{}, Request: req}, []byte(`{"available_count":0,"credits":[]}`), at)
	snapshot, _ = m.Snapshot(ids, codexmonitor.Policy{})
	if snapshot.Accounts[0].Bank != nil {
		t.Fatal("old response revived after action")
	}
	// Wrong account header and unsusbstituted credentials cannot poison cache.
	req.Header.Set("ChatGPT-Account-Id", "wrong-synthetic-account")
	if observe, err := h.prepareMonitorAPICall(a, req, true); observe != nil || err != nil {
		t.Fatal("wrong account accepted")
	}
	req.Header.Del("ChatGPT-Account-Id")
	if observe, err := h.prepareMonitorAPICall(a, req, false); observe != nil || err != nil {
		t.Fatal("supplied credential accepted")
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	// The actual file format can reopen after the action barrier is persisted.
	h.codexMonitor = nil
	m, ids, err = h.monitorInputs()
	if err != nil {
		t.Fatal(err)
	}
	snapshot, _ = m.Snapshot(ids, codexmonitor.Policy{})
	encoded, _ := json.Marshal(snapshot)
	if snapshot.Accounts[0].Bank != nil || strings.Contains(string(encoded), "synthetic-only-token") {
		t.Fatal("restart revived cache or leaked token")
	}
}
