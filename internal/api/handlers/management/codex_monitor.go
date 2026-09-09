package management

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/api/handlers/management/codexmonitor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// The transport is intentionally narrow. Normal management auth protects this
// route, and neither caller-controlled URLs nor token/proxy overrides exist.
const codexMonitorProviderBase = "https://chatgpt.com/backend-api/wham/"
const codexMonitorActivityWindow = time.Hour

func (h *Handler) monitor() (*codexmonitor.Coordinator, error) {
	h.codexMonitorMu.Lock()
	defer h.codexMonitorMu.Unlock()
	if h.codexMonitor != nil || h.codexMonitorInitError != nil {
		return h.codexMonitor, h.codexMonitorInitError
	}
	if h.configFilePath == "" {
		return nil, errors.New("monitor requires an explicit configuration path")
	}
	configPath, err := filepath.Abs(h.configFilePath)
	if err != nil {
		return nil, errors.New("monitor configuration path unavailable")
	}
	store, err := codexmonitor.OpenStore(filepath.Join(filepath.Dir(configPath), "."+filepath.Base(configPath)+".quota-monitor-v1"))
	if err == nil {
		h.codexMonitor, err = codexmonitor.New(store)
	}
	h.codexMonitorInitError = err
	return h.codexMonitor, err
}

func (h *Handler) CloseCodexMonitor() error {
	if h == nil {
		return nil
	}
	h.codexMonitorMu.Lock()
	defer h.codexMonitorMu.Unlock()
	if h.codexMonitor != nil {
		return h.codexMonitor.Close()
	}
	return nil
}

// GetCodexMonitor performs only local observation/cache work. It never probes
// the provider or changes routing, auth, grants, or account cooldowns.
func (h *Handler) GetCodexMonitor(c *gin.Context) {
	monitor, ids, err := h.monitorInputs()
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "quota monitor unavailable; no provider request sent"})
		return
	}
	snapshot, err := monitor.Snapshot(ids, codexmonitor.Policy{UsageSeconds: 1800, ResetSeconds: 86400, GapSeconds: 10})
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "quota monitor cache unavailable; no provider request sent"})
		return
	}
	c.JSON(http.StatusOK, snapshot)
}

func (h *Handler) StepCodexMonitor(c *gin.Context) {
	data, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 4096))
	req, errRequest := codexmonitor.ParseRequest(data)
	if err != nil || errRequest != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid quota monitor request"})
		return
	}
	monitor, ids, err := h.monitorInputs()
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "quota monitor unavailable; no provider request sent"})
		return
	}
	snapshot, err := monitor.Step(c.Request.Context(), ids, req, h.fetchMonitorObservation)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "quota monitor could not safely schedule a check"})
		return
	}
	c.JSON(http.StatusOK, snapshot)
}

func (h *Handler) monitorInputs() (*codexmonitor.Coordinator, []codexmonitor.Identity, error) {
	ids, err := h.monitorIdentities()
	if err != nil {
		return nil, nil, err
	}
	m, err := h.monitor()
	return m, ids, err
}

func codexMonitorIdentityKey(a *coreauth.Auth) string {
	a.EnsureIndex()
	claims := extractCodexIDTokenClaims(a)
	// These selected identity claims are read from the existing in-memory auth
	// owner, hashed immediately and never returned or persisted as plaintext.
	identity, _ := json.Marshal([]any{a.ID, a.Index, a.Provider, authEmail(a),
		a.Metadata["account_id"], a.Metadata["plan_type"], claims["chatgpt_account_id"], claims["plan_type"]})
	digest := sha256.Sum256(identity)
	return hex.EncodeToString(digest[:])
}

func monitorIdentity(a *coreauth.Auth, now time.Time) codexmonitor.Identity {
	key := codexMonitorIdentityKey(a)
	id := codexmonitor.Identity{Key: key, AuthIndex: a.Index,
		Enabled: !a.Disabled && a.Status != coreauth.StatusDisabled, RetryAt: a.NextRetryAfter,
		Passive: codexmonitor.FromSignals(a.Quota.Signals, a.Quota.ObservedAt)}
	if a.Quota.NextRecoverAt.After(id.RetryAt) {
		id.RetryAt = a.Quota.NextRecoverAt
	}
	status := strings.ToLower(a.StatusMessage)
	invalidAuth := strings.Contains(status, "invalid_grant") || strings.Contains(status, "oauth expired")
	if a.LastError != nil {
		invalidAuth = invalidAuth || a.LastError.HTTPStatus == 401 || strings.Contains(strings.ToLower(a.LastError.Code), "invalid_grant")
	}
	// A timed exclusion may recover normally after its deadline. An untimed
	// exclusion or deterministic authentication failure needs owner recovery.
	id.Blocked = invalidAuth || (a.Unavailable && (id.RetryAt.IsZero() || id.RetryAt.After(now)))
	id.Priority, _ = strconv.Atoi(a.Attributes["priority"])
	if a.Attributes["priority"] == "" {
		switch value := a.Metadata["priority"].(type) {
		case float64:
			id.Priority = int(value)
		case int:
			id.Priority = value
		case string:
			id.Priority, _ = strconv.Atoi(value)
		}
	}
	activity := a.RequestActivitySnapshot()
	id.InUse = recentMonitorActivity(activity, now)
	id.Active = id.InUse
	return id
}

func recentMonitorActivity(activity coreauth.RequestActivitySnapshot, now time.Time) bool {
	return activity.InFlight > 0 || !activity.LastCompleted.IsZero() && !activity.LastCompleted.After(now) &&
		now.Sub(activity.LastCompleted) < codexMonitorActivityWindow
}

func (h *Handler) monitorIdentities() ([]codexmonitor.Identity, error) {
	h.mu.Lock()
	manager := h.authManager
	h.mu.Unlock()
	if manager == nil {
		return nil, errors.New("auth manager unavailable")
	}
	ids := []codexmonitor.Identity{}
	now := time.Now()
	for _, a := range manager.List() {
		if a == nil || !strings.EqualFold(a.Provider, "codex") {
			continue
		}
		if len(ids) == codexmonitor.MaxAccounts {
			return nil, errors.New("monitor account capacity exceeded")
		}
		ids = append(ids, monitorIdentity(a, now))
	}
	sort.Slice(ids, func(i, j int) bool {
		if ids[i].Priority != ids[j].Priority {
			return ids[i].Priority > ids[j].Priority
		}
		return ids[i].AuthIndex < ids[j].AuthIndex
	})
	next := 0
	for i := range ids {
		if ids[i].Enabled && !ids[i].Blocked && !ids[i].RetryAt.After(now) {
			if next < 2 {
				ids[i].Active = true
			}
			next++
		}
	}
	return ids, nil
}

func (h *Handler) fetchMonitorObservation(ctx context.Context, id codexmonitor.Identity, kind string) codexmonitor.Result {
	selected := h.monitorAuth(id.AuthIndex, id.Key)
	if selected == nil {
		return codexmonitor.Result{}
	}
	current := monitorIdentity(selected, time.Now())
	if !current.Enabled || current.Blocked || current.RetryAt.After(time.Now()) {
		return codexmonitor.Result{}
	}
	// One shared deadline includes both reads and optional owner recovery. The
	// monitor owns no token parsing, refresh transport, retries or credentials.
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	result := h.fetchMonitorRead(ctx, selected, kind)
	if kind != codexmonitor.UsageLane || result.Status != http.StatusUnauthorized ||
		result.RetryAt.After(time.Now()) || ctx.Err() != nil {
		return result
	}
	h.mu.Lock()
	manager := h.authManager
	h.mu.Unlock()
	if manager == nil {
		return result
	}
	updated, err := manager.RecoverQuotaCredential(ctx, selected)
	if err != nil || updated == nil || updated.RegistrationEpoch != selected.RegistrationEpoch ||
		stringValue(updated.Metadata, "account_id") != stringValue(selected.Metadata, "account_id") {
		return result
	}
	key := codexMonitorIdentityKey(updated)
	updated = h.monitorAuth(id.AuthIndex, key)
	if updated == nil {
		return result
	}
	current = monitorIdentity(updated, time.Now())
	if !current.Enabled || current.Blocked || current.RetryAt.After(time.Now()) || ctx.Err() != nil {
		return result
	}
	result = h.fetchMonitorRead(ctx, updated, kind)
	if key != id.Key {
		// A plan upgrade changes cache authority. Adopt the observation through
		// the existing verified-read boundary under its new identity, never the
		// stale subscription identity captured before credential recovery.
		if monitor, ids, err := h.monitorInputs(); err == nil {
			_ = monitor.ObserveRead(ids, key, kind, result)
		}
	}
	return result
}

func (h *Handler) fetchMonitorRead(ctx context.Context, selected *coreauth.Auth, kind string) codexmonitor.Result {
	path := "usage"
	if kind == codexmonitor.ResetLane {
		path = "rate-limit-reset-credits"
	} else if kind != codexmonitor.UsageLane {
		return codexmonitor.Result{}
	}
	token := tokenValueForAuth(selected)
	if token == "" {
		return codexmonitor.Result{Status: 401}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, codexMonitorProviderBase+path, nil)
	if err != nil {
		return codexmonitor.Result{}
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "CLIProxyAPI-quota-monitor/1")
	if account, ok := selected.Metadata["account_id"].(string); ok && account != "" {
		request.Header.Set("ChatGPT-Account-Id", account)
	}
	h.mu.Lock()
	transport := h.apiCallTransport(selected, "")
	if h.codexMonitorTransport != nil {
		transport = h.codexMonitorTransport(selected)
	}
	h.mu.Unlock()
	if closer, ok := transport.(interface{ CloseIdleConnections() }); ok {
		defer closer.CloseIdleConnections()
	}
	client := &http.Client{Transport: transport, Timeout: 8 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return codexmonitor.Result{}
	}
	defer func() { _ = response.Body.Close() }()
	observedAt := time.Now().UTC()
	result := codexmonitor.Result{Status: response.StatusCode, RetryAt: monitorRetryAfter(response.Header.Get("Retry-After"), observedAt)}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return result // never read, log or cache provider error pages
	}
	data, errRead := io.ReadAll(io.LimitReader(response.Body, codexmonitor.MaxBodyBytes+1))
	if errRead != nil || len(data) > codexmonitor.MaxBodyBytes {
		return result
	}
	if kind == codexmonitor.UsageLane {
		result.Usage, _ = codexmonitor.ParseUsage(data, observedAt)
	} else {
		result.Bank, _ = codexmonitor.ParseBank(data, observedAt)
	}
	return result
}

func (h *Handler) monitorAuth(index, key string) *coreauth.Auth {
	h.mu.Lock()
	manager := h.authManager
	h.mu.Unlock()
	if manager == nil {
		return nil
	}
	var selected *coreauth.Auth
	for _, a := range manager.List() {
		if a == nil || !strings.EqualFold(a.Provider, "codex") || a.EnsureIndex() != index {
			continue
		}
		if selected != nil {
			return nil
		}
		selected = a
	}
	if selected == nil || codexMonitorIdentityKey(selected) != key {
		return nil
	}
	return selected
}

// prepareMonitorAPICall returns a response observer only for the exact Codex
// usage/bank routes with the selected owner's server-substituted token. Generic
// API calls, alternate account headers/hosts, redirects and supplied credentials
// cannot inject cache observations. No provider request is made by this hook.
func (h *Handler) prepareMonitorAPICall(auth *coreauth.Auth, req *http.Request, substituted bool) (func(*http.Response, []byte, time.Time), error) {
	if auth == nil || !strings.EqualFold(auth.Provider, "codex") || !substituted || req == nil ||
		req.URL == nil || req.Header.Get("Authorization") != "Bearer "+tokenValueForAuth(auth) ||
		tokenValueForAuth(auth) == "" || req.Host != "" && req.Host != "chatgpt.com" {
		return nil, nil
	}
	url := req.URL.String()
	kind, mutation := "", false
	switch {
	case req.Method == http.MethodGet && url == codexMonitorProviderBase+"usage":
		kind = codexmonitor.UsageLane
	case req.Method == http.MethodGet && url == codexMonitorProviderBase+"rate-limit-reset-credits":
		kind = codexmonitor.ResetLane
	case req.Method == http.MethodPost && url == codexMonitorProviderBase+"rate-limit-reset-credits/consume":
		mutation = true
	default:
		return nil, nil
	}
	accountHeader := req.Header.Get("ChatGPT-Account-Id")
	if accountHeader != "" && accountHeader != stringValue(auth.Metadata, "account_id") {
		return nil, nil
	}
	key, index := codexMonitorIdentityKey(auth), auth.EnsureIndex()
	if h.monitorAuth(index, key) == nil {
		return nil, errors.New("ambiguous monitor action identity")
	}
	m, ids, err := h.monitorInputs()
	if err != nil {
		if mutation {
			return nil, err
		}
		return nil, nil // optional display observation must not suppress a fresh read
	}
	if mutation {
		if err := m.InvalidateAction(ids, key); err != nil {
			return nil, err
		}
	}
	return func(response *http.Response, data []byte, at time.Time) {
		if h.monitorAuth(index, key) == nil {
			return
		}
		current, err := h.monitorIdentities()
		if err != nil {
			return
		}
		if mutation {
			_ = m.InvalidateAction(current, key)
			return // the result body is not a quota/bank observation
		}
		if response == nil || response.Request == nil || response.Request.URL.String() != url ||
			response.Request.Method != http.MethodGet {
			return
		}
		if at.IsZero() {
			return
		}
		result := codexmonitor.Result{Status: response.StatusCode, RetryAt: monitorRetryAfter(response.Header.Get("Retry-After"), at)}
		if response.StatusCode >= 200 && response.StatusCode < 300 && len(data) <= codexmonitor.MaxBodyBytes {
			if kind == codexmonitor.UsageLane {
				result.Usage, _ = codexmonitor.ParseUsage(data, at)
			} else {
				result.Bank, _ = codexmonitor.ParseBank(data, at)
			}
		}
		_ = m.ObserveRead(current, key, kind, result)
	}, nil
}

func monitorRetryAfter(value string, now time.Time) time.Time {
	if n, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil && n >= 0 && !math.IsNaN(n) {
		if n > float64(math.MaxInt64)/float64(time.Second) {
			return time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
		}
		return now.Add(time.Duration(n * float64(time.Second)))
	}
	if date, err := http.ParseTime(value); err == nil {
		return date
	}
	return time.Time{}
}
