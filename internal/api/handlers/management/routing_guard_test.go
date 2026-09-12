package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func guardedRoutingRequest(method, body, guard string, handler gin.HandlerFunc) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	ctx.Request = httptest.NewRequest(method, "/synthetic-routing", strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	if guard != "" {
		ctx.Request.Header.Set("X-CLIProxy-Routing-Guard", guard)
	}
	handler(ctx)
	return response
}

func TestFamilyRoutingGuardRejectsPriorityAndResetWithoutMutation(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	registered, err := manager.Register(t.Context(), &coreauth.Auth{ID: "guard-test", Provider: "codex", Status: coreauth.StatusActive,
		Metadata: map[string]any{"priority": 100}, Attributes: map[string]string{"priority": "100"}})
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{authManager: manager}
	manager.SetRoutingFamilyConfigured("family-balanced")
	for _, method := range []string{http.MethodPatch, http.MethodPost} {
		handler := h.PatchAuthFileFields
		if method == http.MethodPost {
			handler = h.ResetSessionAffinity
		}
		response := guardedRoutingRequest(method, `{"name":"guard-test","priority":900}`, "legacy", handler)
		if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"error":"family_routing_conflict"`) {
			t.Fatalf("guard response: %d %s", response.Code, response.Body.String())
		}
	}
	after, _ := manager.GetByID(registered.ID)
	if after.Generation != registered.Generation || after.Attributes["priority"] != "100" {
		t.Fatal("rejected priority mutation changed the account")
	}
	manual := guardedRoutingRequest(http.MethodPatch, `{"name":"guard-test","priority":900}`, "", h.PatchAuthFileFields)
	if manual.Code != http.StatusOK {
		t.Fatalf("manual priority edit: %d %s", manual.Code, manual.Body.String())
	}
	invalid := guardedRoutingRequest(http.MethodPatch, `{"name":"guard-test","priority":700}`, "other", h.PatchAuthFileFields)
	if invalid.Code != http.StatusBadRequest {
		t.Fatal("unknown guard was accepted")
	}

	selector := coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{Family: &coreauth.FamilyRoutingConfig{StatePath: filepath.Join(t.TempDir(), "family.json")}})
	defer selector.Stop()
	manager.SetSelector(selector)
	status := decodeSessionAffinityResponse(t, guardedRoutingRequest(http.MethodGet, "", "", h.GetSessionAffinity))
	if status["effective_strategy"] != "family-balanced" || status["routing_guard_supported"] != true || status["family_routing_enabled"] != true {
		t.Fatalf("capability discovery: %+v", status)
	}
}

func TestFamilyRoutingReservationSurvivesOlderReloadAndInvalidSnapshot(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	releaseFirst := manager.ReserveFamilyRoutingConfig("family-balanced")
	releaseSecond := manager.ReserveFamilyRoutingConfig("family-balanced")
	manager.SetRoutingFamilyConfigured("round-robin")
	releaseFirst()
	releaseFirst() // Lease release is idempotent.
	if release, allowed := manager.GuardLegacyRouting(); allowed {
		release()
		t.Fatal("older reload cleared a pending family-mode save")
	}
	h := &Handler{authManager: manager}
	h.reloadConfigAfterManagementSaveAsync(t.Context(), configReloadSnapshot{releaseRouting: releaseSecond})
	release, allowed := manager.GuardLegacyRouting()
	defer release()
	if !allowed {
		t.Fatal("invalid asynchronous snapshot leaked its mode reservation")
	}
}

type blockingRoutingUpdateHook struct {
	coreauth.NoopHook
	entered chan struct{}
	proceed chan struct{}
}

func (h *blockingRoutingUpdateHook) OnAuthUpdated(context.Context, *coreauth.Auth) {
	close(h.entered)
	<-h.proceed
}

func TestFamilyModeActivationWaitsForGuardedPriorityPatch(t *testing.T) {
	hook := &blockingRoutingUpdateHook{entered: make(chan struct{}), proceed: make(chan struct{})}
	manager := coreauth.NewManager(nil, nil, hook)
	if _, err := manager.Register(t.Context(), &coreauth.Auth{ID: "concurrent-guard", Provider: "codex", Status: coreauth.StatusActive}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{authManager: manager}
	patched := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		patched <- guardedRoutingRequest(http.MethodPatch, `{"name":"concurrent-guard","priority":300}`, "legacy", h.PatchAuthFileFields)
	}()
	select {
	case <-hook.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("priority patch never reached its lifecycle hook")
	}
	started, activated := make(chan struct{}), make(chan struct{})
	go func() { close(started); manager.SetRoutingFamilyConfigured("family-balanced"); close(activated) }()
	<-started
	// This is a lock exclusion check, not a TTL, ordering or expiry clock.
	select {
	case <-activated:
		close(hook.proceed)
		t.Fatal("family mode activated inside a guarded mutation")
	case <-time.After(25 * time.Millisecond):
	}
	close(hook.proceed)
	select {
	case <-activated:
	case <-time.After(5 * time.Second):
		t.Fatal("mode activation did not resume after mutation")
	}
	if response := <-patched; response.Code != http.StatusOK {
		t.Fatalf("guarded patch failed: %s", response.Body.String())
	}
	rejected := guardedRoutingRequest(http.MethodPatch, `{"name":"concurrent-guard","priority":500}`, "legacy", h.PatchAuthFileFields)
	if rejected.Code != http.StatusConflict {
		t.Fatal("post-activation patch was admitted")
	}
}
