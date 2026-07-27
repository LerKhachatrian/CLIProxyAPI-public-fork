package management

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type reauthFakeCodexOAuthService struct {
	accountID string
	email     string
}

func (f *reauthFakeCodexOAuthService) GenerateAuthURL(state string, _ *codex.PKCECodes) (string, error) {
	return "https://auth.example.test/oauth?state=" + state, nil
}

func (f *reauthFakeCodexOAuthService) ExchangeCodeForTokens(_ context.Context, code string, _ *codex.PKCECodes) (*codex.CodexAuthBundle, error) {
	now := time.Now().UTC()
	return &codex.CodexAuthBundle{
		TokenData: codex.CodexTokenData{
			IDToken:      "fixture-id-token-" + code,
			AccessToken:  "fixture-access-token-" + code,
			RefreshToken: "fixture-refresh-token-" + code,
			AccountID:    f.accountID,
			Email:        f.email,
			Expire:       now.Add(time.Hour).Format(time.RFC3339),
		},
		LastRefresh: now.Format(time.RFC3339),
	}, nil
}

func (f *reauthFakeCodexOAuthService) CreateTokenStorage(bundle *codex.CodexAuthBundle) *codex.CodexTokenStorage {
	return &codex.CodexTokenStorage{
		IDToken:      bundle.TokenData.IDToken,
		AccessToken:  bundle.TokenData.AccessToken,
		RefreshToken: bundle.TokenData.RefreshToken,
		AccountID:    bundle.TokenData.AccountID,
		LastRefresh:  bundle.LastRefresh,
		Email:        bundle.TokenData.Email,
		Expire:       bundle.TokenData.Expire,
	}
}

type codexReauthFixture struct {
	authDir   string
	filePath  string
	authIndex string
	authID    string
	manager   *coreauth.Manager
	handler   *Handler
	router    http.Handler
}

func newCodexReauthFixture(t *testing.T) *codexReauthFixture {
	t.Helper()
	authDir := filepath.Join(t.TempDir(), "auths")
	if errMkdir := os.MkdirAll(authDir, 0o700); errMkdir != nil {
		t.Fatalf("create auth dir: %v", errMkdir)
	}
	fileName := "codex-target@example.test-pro.json"
	filePath := filepath.Join(authDir, fileName)
	metadata := map[string]any{
		"type":          "codex",
		"email":         "target@example.test",
		"account_id":    "acct-target",
		"id_token":      "fixture-old-id-token",
		"access_token":  "fixture-old-access-token",
		"refresh_token": "fixture-old-refresh-token",
		"prefix":        "team-a",
		"proxy_url":     "http://proxy.example.test:8080",
		"headers": map[string]any{
			"X-Operator": "preserve-me",
		},
		"priority":   17,
		"note":       "operator note",
		"websockets": true,
		"disabled":   false,
		"label":      "Target account",
	}
	raw, errMarshal := json.Marshal(metadata)
	if errMarshal != nil {
		t.Fatalf("marshal fixture auth: %v", errMarshal)
	}
	if errWrite := os.WriteFile(filePath, raw, 0o600); errWrite != nil {
		t.Fatalf("write fixture auth: %v", errWrite)
	}

	record := &coreauth.Auth{
		ID:       fileName,
		Provider: "codex",
		Prefix:   "team-a",
		FileName: fileName,
		Label:    "Target account",
		Status:   coreauth.StatusActive,
		ProxyURL: "http://proxy.example.test:8080",
		Attributes: map[string]string{
			coreauth.AttributePath:          filePath,
			coreauth.AttributeSource:        filePath,
			coreauth.AttributeSourceBackend: coreauth.AuthSourceFile,
			"priority":                      "17",
			"note":                          "operator note",
			"websockets":                    "true",
			"header:X-Operator":             "preserve-me",
		},
		Metadata: metadata,
	}
	authIndex := record.EnsureIndex()
	manager := coreauth.NewManager(nil, nil, nil)
	if _, errRegister := manager.Register(coreauth.WithSkipPersist(context.Background()), record); errRegister != nil {
		t.Fatalf("register fixture auth: %v", errRegister)
	}
	handler := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	handler.tokenStore = sdkAuth.NewFileTokenStore()
	router := gin.New()
	router.GET("/codex-auth-url", handler.RequestCodexToken)
	router.GET("/get-auth-status", handler.GetAuthStatus)
	return &codexReauthFixture{
		authDir:   authDir,
		filePath:  filePath,
		authIndex: authIndex,
		authID:    fileName,
		manager:   manager,
		handler:   handler,
		router:    router,
	}
}

func withReauthFakeCodexService(t *testing.T, accountID, email string) {
	t.Helper()
	original := newCodexOAuthService
	newCodexOAuthService = func(*config.Config) codexOAuthService {
		return &reauthFakeCodexOAuthService{accountID: accountID, email: email}
	}
	t.Cleanup(func() {
		newCodexOAuthService = original
	})
}

func startTargetedCodexReauth(t *testing.T, router http.Handler, authIndex string) string {
	t.Helper()
	requestPath := "/codex-auth-url?auth_index=" + url.QueryEscape(authIndex)
	req := httptest.NewRequest(http.MethodGet, requestPath, nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("start targeted reauth status = %d body=%s", w.Code, w.Body.String())
	}
	var payload struct {
		State string `json:"state"`
		URL   string `json:"url"`
	}
	if errDecode := json.Unmarshal(w.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode targeted reauth response: %v", errDecode)
	}
	if payload.State == "" || !strings.HasPrefix(payload.URL, "https://auth.example.test/") {
		t.Fatalf("invalid targeted reauth response: %s", w.Body.String())
	}
	return payload.State
}

func waitForCodexReauthStatus(t *testing.T, router http.Handler, state string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		req := httptest.NewRequest(http.MethodGet, "/get-auth-status?state="+url.QueryEscape(state), nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("poll targeted reauth status = %d body=%s", w.Code, w.Body.String())
		}
		var payload map[string]any
		if errDecode := json.Unmarshal(w.Body.Bytes(), &payload); errDecode != nil {
			t.Fatalf("decode targeted reauth status: %v", errDecode)
		}
		if payload["status"] != "wait" {
			return payload
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for targeted codex reauth status")
	return nil
}

func TestRequestCodexTokenTargetedReauthReplacesOnlyMatchingAccount(t *testing.T) {
	withReauthFakeCodexService(t, "acct-target", "target@example.test")
	fixture := newCodexReauthFixture(t)
	state := startTargetedCodexReauth(t, fixture.router, fixture.authIndex)
	defer CompleteOAuthSession(state)
	if _, errCallback := WriteOAuthCallbackFileForPendingSession(fixture.authDir, "codex", state, "matching", ""); errCallback != nil {
		t.Fatalf("write targeted callback: %v", errCallback)
	}
	status := waitForCodexReauthStatus(t, fixture.router, state)
	if status["status"] != "ok" || status["result"] != "reconnected" {
		t.Fatalf("unexpected targeted reauth status: %#v", status)
	}

	raw, errRead := os.ReadFile(fixture.filePath)
	if errRead != nil {
		t.Fatalf("read refreshed auth: %v", errRead)
	}
	var persisted map[string]any
	if errDecode := json.Unmarshal(raw, &persisted); errDecode != nil {
		t.Fatalf("decode refreshed auth: %v", errDecode)
	}
	for key, want := range map[string]any{
		"email":        "target@example.test",
		"account_id":   "acct-target",
		"prefix":       "team-a",
		"proxy_url":    "http://proxy.example.test:8080",
		"priority":     float64(17),
		"note":         "operator note",
		"websockets":   true,
		"disabled":     false,
		"label":        "Target account",
		"access_token": "fixture-access-token-matching",
	} {
		if got := persisted[key]; got != want {
			t.Errorf("persisted %s = %#v, want %#v", key, got, want)
		}
	}
	headers, _ := persisted["headers"].(map[string]any)
	if headers["X-Operator"] != "preserve-me" {
		t.Fatalf("custom headers were not preserved: %#v", headers)
	}
	if bytes.Contains(raw, []byte("fixture-old-access-token")) || bytes.Contains(raw, []byte("fixture-old-refresh-token")) {
		t.Fatal("old token material remained in the replaced auth file")
	}
	entries, errReadDir := os.ReadDir(fixture.authDir)
	if errReadDir != nil {
		t.Fatalf("read auth dir: %v", errReadDir)
	}
	jsonCount := 0
	for _, entry := range entries {
		if strings.EqualFold(filepath.Ext(entry.Name()), ".json") {
			jsonCount++
		}
	}
	if jsonCount != 1 {
		t.Fatalf("targeted reauth created %d json auth files, want exactly 1", jsonCount)
	}
	updated, ok := fixture.manager.GetByID(fixture.authID)
	if !ok || updated.Metadata["account_id"] != "acct-target" || updated.Status != coreauth.StatusActive {
		t.Fatalf("runtime auth was not refreshed safely: %#v", updated)
	}
}

func TestRequestCodexTokenTargetedReauthRejectsWrongAccountWithoutMutation(t *testing.T) {
	withReauthFakeCodexService(t, "acct-other", "other@example.test")
	fixture := newCodexReauthFixture(t)
	before, errRead := os.ReadFile(fixture.filePath)
	if errRead != nil {
		t.Fatalf("read original auth: %v", errRead)
	}
	state := startTargetedCodexReauth(t, fixture.router, fixture.authIndex)
	defer CompleteOAuthSession(state)
	if _, errCallback := WriteOAuthCallbackFileForPendingSession(fixture.authDir, "codex", state, "wrong", ""); errCallback != nil {
		t.Fatalf("write targeted callback: %v", errCallback)
	}
	status := waitForCodexReauthStatus(t, fixture.router, state)
	if status["status"] != "error" || status["error"] != "Signed into a different account. Nothing changed." {
		t.Fatalf("unexpected mismatch status: %#v", status)
	}
	after, errRead := os.ReadFile(fixture.filePath)
	if errRead != nil {
		t.Fatalf("read auth after mismatch: %v", errRead)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("wrong-account reauth mutated the target auth file")
	}
	current, ok := fixture.manager.GetByID(fixture.authID)
	if !ok || current.Metadata["account_id"] != "acct-target" {
		t.Fatalf("wrong-account reauth mutated runtime state: %#v", current)
	}
}

func TestRequestCodexTokenTargetValidationFailsClosed(t *testing.T) {
	fixture := newCodexReauthFixture(t)
	tests := []struct {
		name       string
		path       string
		wantStatus int
	}{
		{name: "missing target", path: "/codex-auth-url?auth_index=", wantStatus: http.StatusBadRequest},
		{name: "unknown target", path: "/codex-auth-url?auth_index=missing", wantStatus: http.StatusNotFound},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, test.path, nil)
			w := httptest.NewRecorder()
			fixture.router.ServeHTTP(w, req)
			if w.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d body=%s", w.Code, test.wantStatus, w.Body.String())
			}
		})
	}

	for _, record := range []*coreauth.Auth{
		{
			ID:       "claude.json",
			Provider: "claude",
			FileName: "claude.json",
			Attributes: map[string]string{
				coreauth.AttributePath:          fixture.filePath,
				coreauth.AttributeSource:        fixture.filePath,
				coreauth.AttributeSourceBackend: coreauth.AuthSourceFile,
			},
		},
		{
			ID:       "runtime-codex",
			Provider: "codex",
			Attributes: map[string]string{
				coreauth.AttributeRuntimeOnly: "true",
			},
		},
	} {
		record.EnsureIndex()
		if _, errRegister := fixture.manager.Register(coreauth.WithSkipPersist(context.Background()), record); errRegister != nil {
			t.Fatalf("register invalid target fixture: %v", errRegister)
		}
		req := httptest.NewRequest(http.MethodGet, "/codex-auth-url?auth_index="+url.QueryEscape(record.Index), nil)
		w := httptest.NewRecorder()
		fixture.router.ServeHTTP(w, req)
		if w.Code != http.StatusConflict {
			t.Fatalf("invalid target status = %d, want %d body=%s", w.Code, http.StatusConflict, w.Body.String())
		}
	}
}

func TestTargetedCodexCompletionDoesNotConsumeConcurrentGenericSession(t *testing.T) {
	withReauthFakeCodexService(t, "acct-target", "target@example.test")
	fixture := newCodexReauthFixture(t)
	targetedState := startTargetedCodexReauth(t, fixture.router, fixture.authIndex)
	defer CompleteOAuthSession(targetedState)
	genericState := requestCodexTokenState(t, fixture.router)
	defer CompleteOAuthSession(genericState)
	if _, errCallback := WriteOAuthCallbackFileForPendingSession(fixture.authDir, "codex", targetedState, "targeted", ""); errCallback != nil {
		t.Fatalf("write targeted callback: %v", errCallback)
	}
	status := waitForCodexReauthStatus(t, fixture.router, targetedState)
	if status["status"] != "ok" {
		t.Fatalf("targeted reauth did not complete: %#v", status)
	}
	if !IsOAuthSessionPending(genericState, "codex") {
		t.Fatal("targeted reauth completion consumed the concurrent generic session")
	}
}
