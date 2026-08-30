package access

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestApplyAccessProvidersKeepsAPIKeyAuthAcrossCodexOAuthEnableAndDisable(t *testing.T) {
	for _, providerType := range []string{
		sdkaccess.AccessProviderTypeConfigAPIKey,
		sdkaccess.AccessProviderTypeCodexClientOAuth,
	} {
		sdkaccess.UnregisterProvider(providerType)
	}
	t.Cleanup(func() {
		for _, providerType := range []string{
			sdkaccess.AccessProviderTypeConfigAPIKey,
			sdkaccess.AccessProviderTypeCodexClientOAuth,
		} {
			sdkaccess.UnregisterProvider(providerType)
		}
	})

	manager := sdkaccess.NewManager()
	auths := coreauth.NewManager(nil, nil, nil)
	enabled := &config.Config{Host: "127.0.0.1"}
	enabled.APIKeys = []string{"existing-api-key"}
	enabled.Codex.ClientOAuthAccess.Enabled = true
	if _, errApply := ApplyAccessProviders(manager, auths, nil, enabled); errApply != nil {
		t.Fatalf("ApplyAccessProviders(enable) error = %v", errApply)
	}
	assertProviderIDs(t, manager, sdkaccess.DefaultAccessProviderName, sdkaccess.AccessProviderTypeCodexClientOAuth)
	assertExistingAPIKeyAuthenticates(t, manager)

	disabled := *enabled
	disabled.Codex.ClientOAuthAccess.Enabled = false
	if _, errApply := ApplyAccessProviders(manager, auths, enabled, &disabled); errApply != nil {
		t.Fatalf("ApplyAccessProviders(disable) error = %v", errApply)
	}
	assertProviderIDs(t, manager, sdkaccess.DefaultAccessProviderName)
	assertExistingAPIKeyAuthenticates(t, manager)
}

func assertProviderIDs(t *testing.T, manager *sdkaccess.Manager, expected ...string) {
	t.Helper()
	providers := manager.Providers()
	if len(providers) != len(expected) {
		t.Fatalf("provider count = %d, want %d", len(providers), len(expected))
	}
	for index, provider := range providers {
		if provider == nil || provider.Identifier() != expected[index] {
			t.Fatalf("provider[%d] = %#v, want %q", index, provider, expected[index])
		}
	}
}

func assertExistingAPIKeyAuthenticates(t *testing.T, manager *sdkaccess.Manager) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/v1/models", nil)
	req.Header.Set("Authorization", "Bearer existing-api-key")
	result, authErr := manager.Authenticate(context.Background(), req)
	if authErr != nil {
		t.Fatalf("Authenticate(existing API key) error = %v", authErr)
	}
	if result == nil || result.Provider != sdkaccess.DefaultAccessProviderName {
		t.Fatalf("Authenticate(existing API key) result = %#v", result)
	}
}
