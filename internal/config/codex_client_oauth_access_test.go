package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCodexClientOAuthAccessConfig(t *testing.T) {
	var cfg Config
	if err := yaml.Unmarshal([]byte("host: 127.0.0.1\ncodex:\n  client-oauth-access:\n    enabled: true\n"), &cfg); err != nil {
		t.Fatalf("yaml.Unmarshal() error = %v", err)
	}
	if !cfg.Codex.ClientOAuthAccess.Enabled {
		t.Fatal("client OAuth access was not enabled")
	}

	var defaults Config
	if err := yaml.Unmarshal([]byte("host: 127.0.0.1\n"), &defaults); err != nil {
		t.Fatalf("default yaml.Unmarshal() error = %v", err)
	}
	if defaults.Codex.ClientOAuthAccess.Enabled {
		t.Fatal("client OAuth access must default to disabled")
	}
}
