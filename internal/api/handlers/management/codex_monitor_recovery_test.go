package management

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/api/handlers/management/codexmonitor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type monitorRecoveryExecutor struct {
	coreauth.ProviderExecutor
	calls int
	wait  bool
}

func (e *monitorRecoveryExecutor) Identifier() string { return "codex" }
func (e *monitorRecoveryExecutor) Refresh(ctx context.Context, a *coreauth.Auth) (*coreauth.Auth, error) {
	e.calls++
	if e.wait {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	a.Metadata["access_token"] = "synthetic-recovered"
	a.Metadata["plan_type"] = "pro"
	a.Metadata["expired"] = time.Now().Add(time.Hour).Format(time.RFC3339)
	return a, nil
}

func TestMonitorUsageRecoversOldPlanThroughCredentialOwner(t *testing.T) {
	h, original := monitorTestHandler(t)
	a, _ := h.authManager.GetByID(original.ID)
	a.FileName = "synthetic.json"
	a.Metadata["refresh_token"] = "synthetic-refresh"
	_, _ = h.authManager.Update(context.Background(), a)
	exec := &monitorRecoveryExecutor{}
	h.authManager.RegisterExecutor(exec)
	ids, _ := h.monitorIdentities()
	calls := 0
	h.codexMonitorTransport = func(*coreauth.Auth) http.RoundTripper {
		return monitorRoundTripper(func(req *http.Request) (*http.Response, error) {
			calls++
			status, body := 401, ""
			if req.Header.Get("Authorization") == "Bearer synthetic-recovered" {
				status, body = 200, `{"plan_type":"pro","rate_limit":{"secondary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_after_seconds":604000}}}`
			}
			return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
		})
	}
	result := h.fetchMonitorObservation(context.Background(), ids[0], codexmonitor.UsageLane)
	if calls != 2 || exec.calls != 1 || result.Usage == nil || result.Usage.Plan != "pro" || result.Usage.Windows[0].UsedPercent != 0 {
		t.Fatal("post-upgrade usage was not recovered", calls, exec.calls)
	}
	m, current, err := h.monitorInputs()
	if err != nil || current[0].Key == ids[0].Key {
		t.Fatal("subscription identity did not update", err)
	}
	snapshot, err := m.Snapshot(current, codexmonitor.Policy{})
	if err != nil || snapshot.Accounts[0].Usage == nil || snapshot.Accounts[0].Usage.Plan != "pro" || snapshot.Accounts[0].Bank != nil {
		t.Fatal("new usage was lost or reset inventory was fetched", err)
	}
}

func TestMonitorUsageRecoveryAfterRoutingUnauthorized(t *testing.T) {
	for _, mode := range []string{"elapsed_retry", "future_retry", "untimed", "invalid_grant_code", "invalid_grant_status", "oauth_expired", "quota_retry", "disabled", "owner_cooldown"} {
		t.Run(mode, func(t *testing.T) {
			h, original := monitorTestHandler(t)
			a, _ := h.authManager.GetByID(original.ID)
			a.FileName = "synthetic.json"
			a.Metadata["refresh_token"] = "synthetic-refresh"
			a.Status = coreauth.StatusError
			a.Unavailable = true
			a.StatusMessage = "authentication_error: access token invalidated"
			a.LastError = &coreauth.Error{HTTPStatus: http.StatusUnauthorized, Code: "authentication_error"}
			a.NextRetryAfter = time.Now().Add(-time.Minute)
			wantReads, wantRefresh := 0, 0
			switch mode {
			case "elapsed_retry":
				wantReads, wantRefresh = 2, 1
			case "future_retry":
				a.NextRetryAfter = time.Now().Add(time.Hour)
			case "untimed":
				a.NextRetryAfter = time.Time{}
			case "invalid_grant_code":
				a.LastError.Code = "invalid_grant"
			case "invalid_grant_status":
				a.StatusMessage = "invalid_grant"
			case "oauth_expired":
				a.StatusMessage = "oauth expired"
			case "quota_retry":
				a.Quota.NextRecoverAt = time.Now().Add(time.Hour)
			case "disabled":
				a.Disabled = true
			case "owner_cooldown":
				a.NextRefreshAfter = time.Now().Add(time.Hour)
				wantReads = 1
			}
			if _, err := h.authManager.Update(context.Background(), a); err != nil {
				t.Fatal(err)
			}
			executor := &monitorRecoveryExecutor{}
			h.authManager.RegisterExecutor(executor)
			reads := 0
			h.codexMonitorTransport = func(*coreauth.Auth) http.RoundTripper {
				return monitorRoundTripper(func(req *http.Request) (*http.Response, error) {
					reads++
					if req.URL.Path != "/backend-api/wham/usage" {
						t.Fatal("usage recovery attempted another endpoint")
					}
					status, body := http.StatusUnauthorized, ""
					if req.Header.Get("Authorization") == "Bearer synthetic-recovered" {
						status, body = http.StatusOK, `{"plan_type":"pro","rate_limit":{"secondary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_after_seconds":604000}}}`
					}
					return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
				})
			}
			monitor, ids, err := h.monitorInputs()
			if err != nil {
				t.Fatal(err)
			}
			_, err = monitor.Step(context.Background(), ids, codexmonitor.Request{
				Policy: codexmonitor.Policy{GapSeconds: 10}, Refresh: codexmonitor.UsageLane, AuthIndex: a.Index,
			}, h.fetchMonitorObservation)
			if err != nil {
				t.Fatal(err)
			}
			if reads != wantReads || executor.calls != wantRefresh {
				t.Fatalf("reads/renewals = %d/%d, want %d/%d", reads, executor.calls, wantReads, wantRefresh)
			}
			_, current, err := h.monitorInputs()
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := monitor.Snapshot(current, codexmonitor.Policy{})
			if err != nil {
				t.Fatal(err)
			}
			if mode == "disabled" {
				if len(snapshot.Accounts) != 1 || snapshot.Accounts[0].Usage != nil || snapshot.Accounts[0].Bank != nil {
					t.Fatal("disabled account retained observation authority")
				}
				return
			}
			if len(snapshot.Accounts) != 1 || snapshot.Accounts[0].Bank != nil {
				t.Fatal("account identity or reset isolation changed")
			}
			usage := snapshot.Accounts[0].Usage
			if wantRefresh == 1 {
				if usage == nil || usage.Plan != "pro" || len(usage.Windows) != 1 || usage.Windows[0].UsedPercent != 0 || current[0].Key == ids[0].Key {
					t.Fatal("renewed plan observation was not adopted under the new identity")
				}
			} else if usage != nil {
				t.Fatal("failed or excluded recovery manufactured a usage observation")
			}
		})
	}
}

func TestMonitorRecoveryIsUsageOnlyHonorsRetryAfterAndDeadline(t *testing.T) {
	for _, mode := range []string{"reset", "retry_after", "forbidden", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			h, original := monitorTestHandler(t)
			a, _ := h.authManager.GetByID(original.ID)
			a.FileName = "synthetic.json"
			a.Metadata["refresh_token"] = "synthetic-refresh"
			_, _ = h.authManager.Update(context.Background(), a)
			exec := &monitorRecoveryExecutor{wait: mode == "deadline"}
			h.authManager.RegisterExecutor(exec)
			ids, _ := h.monitorIdentities()
			calls := 0
			h.codexMonitorTransport = func(*coreauth.Auth) http.RoundTripper {
				return monitorRoundTripper(func(req *http.Request) (*http.Response, error) {
					calls++
					header, status := http.Header{}, 401
					if mode == "retry_after" {
						header.Set("Retry-After", "120")
					}
					if mode == "forbidden" {
						status = 403
					}
					return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
				})
			}
			kind := codexmonitor.UsageLane
			if mode == "reset" {
				kind = codexmonitor.ResetLane
			}
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			result := h.fetchMonitorObservation(ctx, ids[0], kind)
			wantRefresh := 0
			if mode == "deadline" {
				wantRefresh = 1
			}
			if calls != 1 || exec.calls != wantRefresh || result.Usage != nil || result.Bank != nil {
				t.Fatal("recovery scope/deadline was bypassed")
			}
		})
	}
}
