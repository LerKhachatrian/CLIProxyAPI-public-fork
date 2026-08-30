package cliproxy

import (
	"strings"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
)

func TestBuilderBuildFailsClosedForCodexClientOAuthOnNonLoopbackHost(t *testing.T) {
	sdkaccess.UnregisterProvider(sdkaccess.AccessProviderTypeCodexClientOAuth)
	t.Cleanup(func() {
		sdkaccess.UnregisterProvider(sdkaccess.AccessProviderTypeCodexClientOAuth)
	})

	cfg := &internalconfig.Config{Host: "0.0.0.0"}
	cfg.Codex.ClientOAuthAccess.Enabled = true
	service, errBuild := NewBuilder().
		WithConfig(cfg).
		WithConfigPath(t.TempDir() + "/config.yaml").
		Build()
	if errBuild == nil {
		t.Fatal("Build() enabled Codex client OAuth on a non-loopback host")
	}
	if service != nil {
		t.Fatal("Build() returned a service for unsafe Codex client OAuth configuration")
	}
	if !strings.Contains(errBuild.Error(), "requires an explicit loopback host") {
		t.Fatalf("Build() error = %q, want explicit loopback failure", errBuild)
	}
}

func TestBuilderBuildRegistersCodexClientOAuthForLoopbackHost(t *testing.T) {
	sdkaccess.UnregisterProvider(sdkaccess.AccessProviderTypeCodexClientOAuth)
	t.Cleanup(func() {
		sdkaccess.UnregisterProvider(sdkaccess.AccessProviderTypeCodexClientOAuth)
	})

	cfg := &internalconfig.Config{Host: "127.0.0.1"}
	cfg.Codex.ClientOAuthAccess.Enabled = true
	service, errBuild := NewBuilder().
		WithConfig(cfg).
		WithConfigPath(t.TempDir() + "/config.yaml").
		Build()
	if errBuild != nil {
		t.Fatalf("Build() error = %v", errBuild)
	}
	if service == nil {
		t.Fatal("Build() returned nil service")
	}
	found := false
	for _, provider := range service.accessManager.Providers() {
		if provider != nil && provider.Identifier() == sdkaccess.AccessProviderTypeCodexClientOAuth {
			found = true
		}
	}
	if !found {
		t.Fatal("Codex client OAuth provider was not registered")
	}
}
