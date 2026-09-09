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
	usageCalls := 0
	h.codexMonitorTransport = func(*coreauth.Auth) http.RoundTripper {
		return monitorRoundTripper(func(req *http.Request) (*http.Response, error) {
			mu.Lock()
			defer mu.Unlock()
			starts = append(starts, time.Now())
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
		t.Fatalf("Widget contract: %v\n%.8000s", err, output.String())
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	var receipt map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &receipt); err != nil || receipt["status"] != "passed" {
		t.Fatal("missing Widget contract receipt")
	}
	t.Logf("WIDGET_E2E %s", lines[len(lines)-1])
}
