package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestSessionAffinityHandlersReportAndResetBindings(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := cliproxyauth.NewManager(nil, nil, nil)
	selector := cliproxyauth.NewSessionAffinitySelectorWithConfig(cliproxyauth.SessionAffinityConfig{
		Fallback: &cliproxyauth.RoundRobinSelector{},
		TTL:      time.Hour,
	})
	defer selector.Stop()
	manager.SetSelector(selector)

	if _, errPick := selector.Pick(
		t.Context(),
		"codex",
		"gpt-test",
		cliproxyexecutor.Options{Metadata: map[string]any{
			cliproxyexecutor.DerivedSessionIDMetadataKey: "handler-session",
		}},
		[]*cliproxyauth.Auth{{ID: "auth-a", Provider: "codex", Status: cliproxyauth.StatusActive}},
	); errPick != nil {
		t.Fatalf("seed Pick(): %v", errPick)
	}
	handler := &Handler{authManager: manager}

	status := invokeSessionAffinityHandler(t, http.MethodGet, handler.GetSessionAffinity)
	if status.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want %d: %s", status.Code, http.StatusOK, status.Body.String())
	}
	statusPayload := decodeSessionAffinityResponse(t, status)
	if enabled, _ := statusPayload["enabled"].(bool); !enabled {
		t.Fatalf("GET enabled = %#v, want true", statusPayload["enabled"])
	}
	if keys := int(statusPayload["session_keys"].(float64)); keys != 1 {
		t.Fatalf("GET session_keys = %d, want 1", keys)
	}

	reset := invokeSessionAffinityHandler(t, http.MethodPost, handler.ResetSessionAffinity)
	if reset.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want %d: %s", reset.Code, http.StatusOK, reset.Body.String())
	}
	resetPayload := decodeSessionAffinityResponse(t, reset)
	if cleared := int(resetPayload["cleared_session_keys"].(float64)); cleared != 1 {
		t.Fatalf("POST cleared_session_keys = %d, want 1", cleared)
	}

	repeat := invokeSessionAffinityHandler(t, http.MethodPost, handler.ResetSessionAffinity)
	if repeat.Code != http.StatusOK {
		t.Fatalf("repeat POST status = %d, want %d: %s", repeat.Code, http.StatusOK, repeat.Body.String())
	}
	repeatPayload := decodeSessionAffinityResponse(t, repeat)
	if cleared := int(repeatPayload["cleared_session_keys"].(float64)); cleared != 0 {
		t.Fatalf("repeat POST cleared_session_keys = %d, want 0", cleared)
	}
}

func TestResetSessionAffinityReturnsConflictWhenDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := &Handler{authManager: cliproxyauth.NewManager(nil, nil, nil)}

	response := invokeSessionAffinityHandler(t, http.MethodPost, handler.ResetSessionAffinity)
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusConflict, response.Body.String())
	}
	payload := decodeSessionAffinityResponse(t, response)
	if payload["error"] != "session affinity is not enabled" {
		t.Fatalf("error = %#v", payload["error"])
	}
}

func TestSessionAffinityHandlersReturnServiceUnavailableWithoutManager(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := &Handler{}

	for _, testCase := range []struct {
		name   string
		method string
		call   gin.HandlerFunc
	}{
		{name: "get", method: http.MethodGet, call: handler.GetSessionAffinity},
		{name: "reset", method: http.MethodPost, call: handler.ResetSessionAffinity},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			response := invokeSessionAffinityHandler(t, testCase.method, testCase.call)
			if response.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusServiceUnavailable, response.Body.String())
			}
		})
	}
}

func invokeSessionAffinityHandler(t *testing.T, method string, handler gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	ctx.Request = httptest.NewRequest(method, "/v0/management/routing/session-affinity", nil)
	handler(ctx)
	return response
}

func decodeSessionAffinityResponse(t *testing.T, response *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	payload := make(map[string]any)
	if errDecode := json.Unmarshal(response.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	return payload
}
