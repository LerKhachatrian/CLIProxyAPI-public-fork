package cliproxy

import (
	"path/filepath"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestFamilyRoutingConfigUsesExplicitModeAndConfigRelativeState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &internalconfig.Config{Routing: internalconfig.RoutingConfig{Strategy: "family-balanced"}}
	state := normalizedRoutingRuntimeState(cfg).withConfigPath(path)
	if !state.sessionAffinity || state.family.StatePath != filepath.Join(filepath.Dir(path), ".config.yaml.family-routing-v1.json") ||
		state.family.MaxConcurrentPerAccount != 4 || state.family.MaxQueuedPerAccount != 32 || state.family.AdmissionTimeout != 2*time.Minute {
		t.Fatalf("default family configuration: %+v", state)
	}
	cfg.Routing.Family.StateFile = filepath.Join("routing", "families.json")
	state = normalizedRoutingRuntimeState(cfg).withConfigPath(path)
	if state.family.StatePath != filepath.Join(filepath.Dir(path), "routing", "families.json") {
		t.Fatal("state path depends on launch cwd")
	}
	cfg.Routing.Strategy = "round-robin"
	state = normalizedRoutingRuntimeState(cfg).withConfigPath(path)
	if state.sessionAffinity || state.family.StatePath != "" {
		t.Fatal("legacy mode implicitly enabled family routing")
	}
}

func TestFamilyRoutingHotReloadRetainsOwnerAndLegacyRollbackReopensState(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	defer manager.SetSelector(nil)
	service := &Service{coreManager: manager, configPath: filepath.Join(t.TempDir(), "config.yaml")}
	cfg := &internalconfig.Config{Routing: internalconfig.RoutingConfig{Strategy: "family-balanced"}}
	if !service.applyManagerConfig(t.Context(), configCommit{cfg: cfg, sequence: 1}) {
		t.Fatal("initial family activation failed")
	}
	original := manager.Selector()
	cfg.Routing.Family.MaxConcurrentPerAccount = 2
	if !service.applyManagerConfig(t.Context(), configCommit{cfg: cfg, sequence: 2}) || manager.Selector() != original {
		t.Fatal("settings reload replaced the lifetime state owner")
	}
	if status, enabled := manager.FamilyRoutingStatus(); !enabled || !status.PersistenceHealthy || status.MaxConcurrentPerAccount != 2 {
		t.Fatalf("hot settings: %+v", status)
	}
	cfg.Routing.Family.StateFile = "different.json"
	if service.applyManagerConfig(t.Context(), configCommit{cfg: cfg, sequence: 3}) || manager.Selector() != original {
		t.Fatal("hot state relocation was accepted")
	}
	cfg.Routing.Family.StateFile = ""
	cfg.Routing.Strategy = "round-robin"
	if !service.applyManagerConfig(t.Context(), configCommit{cfg: cfg, sequence: 4}) || manager.FamilyRoutingEnabled() {
		t.Fatal("legacy strategy rollback failed")
	}
	cfg.Routing.Strategy = "family-balanced"
	if !service.applyManagerConfig(t.Context(), configCommit{cfg: cfg, sequence: 5}) {
		t.Fatal("reactivation could not acquire old state lock")
	}
	if status, _ := manager.FamilyRoutingStatus(); !status.PersistenceHealthy {
		t.Fatal("reactivation retained a closed writer")
	}
}
