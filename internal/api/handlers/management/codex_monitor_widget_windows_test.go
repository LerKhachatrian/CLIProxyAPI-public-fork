package management

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/api/handlers/management/codexmonitor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// Opt-in real cross-repo contract. Every account, transport and state directory
// is synthetic. The native Windows subprocess is hidden and owned to completion.
func TestCodexMonitorWidgetE2E(t *testing.T) {
	root := os.Getenv("CODEX_MONITOR_WIDGET_SOURCE")
	if root == "" {
		t.Skip("Set CODEX_MONITOR_WIDGET_SOURCE to the accepted Widget source export")
	}
	if !filepath.IsAbs(root) {
		t.Fatal("absolute accepted source required")
	}
	script := filepath.Join(root, "scripts", "verify_quota_monitor.py")
	if _, err := os.Stat(script); err != nil {
		t.Fatal(err)
	}
	h, a := monitorTestHandler(t)
	now := time.Now().UTC()
	a.Quota = coreauth.QuotaState{ObservedAt: now, Signals: map[string]string{
		"x-codex-secondary-used-percent": "40", "x-codex-secondary-window-minutes": "10080",
		"x-codex-secondary-reset-after-seconds": "400000", "x-codex-plan-type": "pro"}}
	if _, err := h.authManager.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	b := &coreauth.Auth{ID: "synthetic-second", Provider: "codex", Status: coreauth.StatusActive,
		Metadata:   map[string]any{"access_token": "synthetic-second-token", "account_id": "synthetic-second-account"},
		Attributes: map[string]string{"priority": "0"}, Quota: coreauth.QuotaState{ObservedAt: now, Signals: map[string]string{
			"x-codex-primary-used-percent": "100", "x-codex-primary-window-minutes": "300", "x-codex-primary-reset-after-seconds": "10000",
			"x-codex-secondary-used-percent": "50", "x-codex-secondary-window-minutes": "10080", "x-codex-secondary-reset-after-seconds": "400000", "x-codex-plan-type": "pro"}}}
	if _, err := h.authManager.Register(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var starts []time.Time
	var admitted []time.Time
	usageCalls := 0
	h.codexMonitorTransport = func(selected *coreauth.Auth) http.RoundTripper {
		return monitorRoundTripper(func(req *http.Request) (*http.Response, error) {
			arrived := time.Now()
			// Observe the persisted scheduling claim separately from entry into
			// this synthetic transport; intervening fsync cost is not constant.
			monitor, ids, err := h.monitorInputs()
			if err != nil {
				return nil, err
			}
			snapshot, err := monitor.Snapshot(ids, codexmonitor.Policy{})
			if err != nil {
				return nil, err
			}
			var claim time.Time
			for _, row := range snapshot.Accounts {
				if row.AuthIndex == selected.Index {
					claim = row.ResetSchedule.LastAttempt
				}
			}
			mu.Lock()
			defer mu.Unlock()
			starts = append(starts, arrived)
			admitted = append(admitted, claim)
			if strings.HasSuffix(req.URL.Path, "/usage") {
				usageCalls++
			}
			if req.Method != "GET" {
				return nil, fmt.Errorf("fixture refuses mutation")
			}
			return &http.Response{StatusCode: 200, Header: http.Header{}, Request: req, Body: io.NopCloser(strings.NewReader(`{"available_count":0,"credits":[]}`))}, nil
		})
	}
	nonce := uuid.NewString()
	router := gin.New()
	router.GET("/v0/management/auth-files", func(c *gin.Context) {
		ids, err := h.monitorIdentities()
		if err != nil {
			c.Status(503)
			return
		}
		rows := []any{}
		for i, id := range ids {
			rows = append(rows, gin.H{"auth_index": id.AuthIndex, "quota_monitor_identity": id.Key, "provider": "codex", "label": fmt.Sprintf("Synthetic %d", i), "priority": id.Priority})
		}
		c.JSON(200, gin.H{"files": rows})
	})
	router.GET("/v0/management/codex/quota-monitor", h.GetCodexMonitor)
	router.POST("/v0/management/codex/quota-monitor", h.StepCodexMonitor)
	fixture := func(c *gin.Context) {
		if c.GetHeader("X-Fixture-Nonce") != nonce {
			c.Status(403)
			return
		}
		if c.Request.Method == "POST" {
			if err := h.CloseCodexMonitor(); err != nil {
				c.Status(409)
				return
			}
			h.codexMonitorMu.Lock()
			h.codexMonitor = nil
			h.codexMonitorMu.Unlock()
		}
		mu.Lock()
		defer mu.Unlock()
		gap := int64(0)
		if len(starts) > 1 {
			gap = starts[1].Sub(starts[0]).Milliseconds()
		}
		c.JSON(200, gin.H{"fixture": "codex-monitor-widget-v1", "nonce": nonce, "provider_calls": len(starts), "usage_calls": usageCalls, "min_gap_ms": gap})
	}
	router.GET("/__quota-monitor-fixture", fixture)
	router.POST("/__quota-monitor-fixture", fixture)
	server := httptest.NewServer(router)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "python", "-X", "utf8", script, "--base-url", server.URL, "--fixture-nonce", nonce, "--qa-profile-root", filepath.Join(t.TempDir(), "profile"))
	cmd.Dir = root
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Run(); err != nil {
		mu.Lock()
		if len(starts) == 2 && len(admitted) == 2 {
			t.Logf("WIDGET_PACING_DIAGNOSTIC admission_gap=%s arrival_gap=%s first_offset=%s second_offset=%s",
				admitted[1].Sub(admitted[0]), starts[1].Sub(starts[0]), starts[0].Sub(admitted[0]), starts[1].Sub(admitted[1]))
		}
		mu.Unlock()
		t.Fatalf("Widget contract: %v\n%.8000s", err, output.String())
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	var receipt map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &receipt); err != nil || receipt["status"] != "passed" {
		t.Fatal("missing Widget contract receipt")
	}
	t.Logf("WIDGET_E2E %s", lines[len(lines)-1])
}

// The existing cross-repo owner also proves the automatic bootstrap through
// actual Widget clients and its banked-reset card. No Widget production change
// or alternate poller is required. Real pacing is deliberately not accelerated.
func TestCodexMonitorInitialInventoryWidgetE2E(t *testing.T) {
	root := os.Getenv("CODEX_MONITOR_WIDGET_SOURCE")
	if root == "" {
		t.Skip("Set CODEX_MONITOR_WIDGET_SOURCE to the accepted Widget source export")
	}
	if !filepath.IsAbs(root) {
		t.Fatal("absolute accepted Widget source required")
	}
	h, first := monitorTestHandler(t)
	if _, err := h.authManager.Register(context.Background(), &coreauth.Auth{
		ID: "synthetic-initial-second", Provider: "codex", Status: coreauth.StatusActive,
		Metadata:   map[string]any{"access_token": "synthetic-second-token", "account_id": "synthetic-second-account"},
		Attributes: map[string]string{"priority": "0"},
	}); err != nil {
		t.Fatal(err)
	}
	// Seed only this unopened, test-owned cache with the old strict v1 entries.
	// The release must upgrade existing never-attempted deadlines, not merely
	// fix brand-new entries. No live or currently locked cache is ever edited.
	ids, err := h.monitorIdentities()
	if err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(filepath.Dir(h.configFilePath), "."+filepath.Base(h.configFilePath)+".quota-monitor-v1")
	if err := os.Mkdir(cache, 0700); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		entry := codexmonitor.Entry{Schema: 1, Key: id.Key,
			ResetSchedule: codexmonitor.Lane{DueAt: time.Now().Add(23 * time.Hour)}}
		data, err := json.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cache, id.Key+".json"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	var starts []time.Time
	seen := map[string]bool{}
	h.codexMonitorTransport = func(selected *coreauth.Auth) http.RoundTripper {
		return monitorRoundTripper(func(req *http.Request) (*http.Response, error) {
			mu.Lock()
			defer mu.Unlock()
			if req.Method != http.MethodGet || req.URL.String() != codexMonitorProviderBase+"rate-limit-reset-credits" || seen[selected.ID] {
				return nil, fmt.Errorf("synthetic inventory refuses usage, mutation or duplicate reads")
			}
			seen[selected.ID] = true
			starts = append(starts, time.Now())
			count := 15
			if selected.ID == first.ID {
				count = 4
			}
			credits := []any{}
			for i := 0; i < count; i++ {
				credits = append(credits, gin.H{"id": fmt.Sprintf("synthetic-credit-%d", i), "reset_type": "codex_rate_limits", "status": "available",
					"granted_at": time.Now().Add(-time.Hour).UTC(), "expires_at": time.Now().Add(24 * time.Hour).UTC()})
			}
			body, err := json.Marshal(gin.H{"available_count": count, "credits": credits})
			if err != nil {
				return nil, err
			}
			return &http.Response{StatusCode: 200, Header: http.Header{}, Request: req, Body: io.NopCloser(bytes.NewReader(body))}, nil
		})
	}
	nonce := uuid.NewString()
	router := gin.New()
	router.GET("/v0/management/auth-files", func(c *gin.Context) {
		rows := []any{}
		for i, id := range ids {
			rows = append(rows, gin.H{"auth_index": id.AuthIndex, "quota_monitor_identity": id.Key, "provider": "codex", "label": fmt.Sprintf("Synthetic %d", i), "priority": id.Priority})
		}
		c.JSON(200, gin.H{"files": rows})
	})
	router.GET("/v0/management/codex/quota-monitor", h.GetCodexMonitor)
	router.POST("/v0/management/codex/quota-monitor", h.StepCodexMonitor)
	fixture := func(c *gin.Context) {
		if c.GetHeader("X-Fixture-Nonce") != nonce {
			c.Status(403)
			return
		}
		if c.Request.Method == http.MethodPost {
			if err := h.CloseCodexMonitor(); err != nil {
				c.Status(409)
				return
			}
			h.codexMonitorMu.Lock()
			h.codexMonitor = nil
			h.codexMonitorMu.Unlock()
		}
		mu.Lock()
		defer mu.Unlock()
		gap := int64(0)
		if len(starts) == 2 {
			gap = starts[1].Sub(starts[0]).Milliseconds()
		}
		c.JSON(200, gin.H{"fixture": "codex-monitor-initial-v1", "nonce": nonce, "provider_calls": len(starts), "min_gap_ms": gap})
	}
	router.GET("/__initial-inventory-fixture", fixture)
	router.POST("/__initial-inventory-fixture", fixture)
	server := httptest.NewServer(router)
	defer server.Close()
	const script = `
import json, os, pathlib, sys, threading, time, urllib.request
root, base, nonce, profile = sys.argv[1:]
sys.path.insert(0, root)
from urllib.parse import urlsplit
url = urlsplit(base)
assert url.scheme == 'http' and url.hostname == '127.0.0.1' and url.port not in (48317, 48318, 48319)
assert pathlib.Path(profile).is_absolute() and not pathlib.Path(profile).exists()
os.environ['QT_QPA_PLATFORM'] = 'offscreen'
opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
def evidence(method='GET'):
    req = urllib.request.Request(base+'/__initial-inventory-fixture', method=method, headers={'X-Fixture-Nonce':nonce})
    with opener.open(req, timeout=3) as response:
        raw = response.read(4097)
    assert len(raw) <= 4096
    data = json.loads(raw)
    assert data['fixture'] == 'codex-monitor-initial-v1' and data['nonce'] == nonce
    return data
assert evidence()['provider_calls'] == 0
from app.qa_profile import activate_qa_profile
activate_qa_profile(profile)
from app.cliproxy_quota_client import CLIProxyQuotaClient
from app.quota_models import build_banked_reset_summary
from app.ui.main_window import MainWindow
from PySide6.QtWidgets import QApplication
app = QApplication.instance() or QApplication([])
clients = [CLIProxyQuotaClient(base_url=base, management_key='synthetic-fixture-only') for _ in range(3)]
for client in clients:
    client.configure_polling(gap_ms=10000, stop_event=threading.Event(), usage_ms=0, reset_ms=0)
    assert build_banked_reset_summary(client.fetch()).reporting_account_count == 0
assert evidence()['provider_calls'] == 0
window = MainWindow(start_metrics=False, start_quota=False, start_usage_analytics=False,
    start_reset_center=False, start_recent_projection=False, load_preferences=False, persist_preferences=False)
try:
    for client in clients:
        client.configure_polling(gap_ms=10000, stop_event=threading.Event(), usage_ms=0, reset_ms=86400000)
    first = clients[0].fetch()
    partial = build_banked_reset_summary(first)
    assert partial.known_resets == 4 and partial.reporting_account_count == 1 and not partial.complete
    window._on_quota_snapshot(first)
    assert window.banked_resets_card.primary_label.text() == '~\u22654'
    before = evidence()
    assert before['provider_calls'] == 1
    evidence('POST')
    deadline = time.monotonic()+130
    polls = 0
    while True:
        current = clients[polls % 3].fetch()
        summary = build_banked_reset_summary(current)
        if summary.complete:
            break
        assert time.monotonic() < deadline
        polls += 1
        time.sleep(1)
    assert summary.known_resets == 19 and summary.reporting_account_count == 2
    window._on_quota_snapshot(current)
    assert window.banked_resets_card.primary_label.text() == '19'
    final = evidence()
    assert final['provider_calls'] == 2 and final['min_gap_ms'] >= 60000
    captures = tuple(a.reset_observed_at for a in current.accounts)
    evidence('POST')
    for client in clients:
        replay = client.fetch()
        assert tuple(a.reset_observed_at for a in replay.accounts) == captures
    assert evidence()['provider_calls'] == 2
finally:
    window.close()
    app.processEvents()
print(json.dumps({'status':'passed', 'clients':3, 'legacy_entries':2, 'synthetic_total':19,
    'initial_partial':4, 'min_gap_ms':final['min_gap_ms'], 'provider_reads':2,
    'manual_intents':0, 'real_provider_calls':0, 'restarts_preserved':True, 'offscreen_ui_verified':True}))
`
	ctx, cancel := context.WithTimeout(context.Background(), 155*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "python", "-X", "utf8", "-c", script, root, server.URL, nonce, filepath.Join(t.TempDir(), "profile"))
	cmd.Dir = root
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Run(); err != nil {
		t.Fatalf("Widget initial inventory contract: %v\n%.8000s", err, output.String())
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	var receipt map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &receipt); err != nil || receipt["status"] != "passed" {
		t.Fatal("missing initial inventory Widget receipt")
	}
	t.Logf("WIDGET_INITIAL_INVENTORY_E2E %s", lines[len(lines)-1])
}
