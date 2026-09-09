package management

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/api/handlers/management/codexmonitor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type monitorRoundTripper func(*http.Request) (*http.Response, error)

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
