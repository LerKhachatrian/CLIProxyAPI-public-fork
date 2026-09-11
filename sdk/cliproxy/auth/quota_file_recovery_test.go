package auth_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/watcher/synthesizer"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type fileQuotaRecoveryExecutor struct {
	coreauth.ProviderExecutor
	calls int
}

func (*fileQuotaRecoveryExecutor) Identifier() string { return "codex" }

func (e *fileQuotaRecoveryExecutor) Refresh(_ context.Context, a *coreauth.Auth) (*coreauth.Auth, error) {
	e.calls++
	updated := a.Clone()
	updated.Metadata["access_token"] = "synthetic-recovered"
	updated.Metadata["expired"] = time.Now().Add(time.Hour).Format(time.RFC3339)
	return updated, nil
}

func synthesizedQuotaAuth(t *testing.T) *coreauth.Auth {
	t.Helper()
	dir := t.TempDir()
	data := []byte(`{"type":"codex","email":"synthetic@example.invalid","account_id":"synthetic-file-account","access_token":"synthetic-old","refresh_token":"synthetic-refresh","expired":"2099-01-01T00:00:00Z"}`)
	if err := os.WriteFile(filepath.Join(dir, "synthetic.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	entries, err := synthesizer.NewFileSynthesizer().Synthesize(&synthesizer.SynthesisContext{
		Config: &config.Config{}, AuthDir: dir, Now: time.Now(),
		IDGenerator: synthesizer.NewStableIDGenerator(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected one synthesized auth, got %d", len(entries))
	}
	a := entries[0]
	if a.FileName != "" || a.AuthSourceKind() != coreauth.AuthSourceFile {
		t.Fatal("fixture must exercise the real attribute-backed loader without FileName")
	}
	return a
}

func TestQuotaRecoveryUsesRealFileLoader(t *testing.T) {
	a := synthesizedQuotaAuth(t)
	m := coreauth.NewManager(nil, nil, nil)
	executor := &fileQuotaRecoveryExecutor{}
	m.RegisterExecutor(executor)
	selected, err := m.Register(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		recovered, err := m.RecoverQuotaCredential(context.Background(), selected)
		if err != nil {
			t.Fatal("attribute-backed file auth must recover through its owner:", err)
		}
		if recovered.ID != selected.ID || recovered.AuthSourceKind() != coreauth.AuthSourceFile ||
			recovered.Metadata["access_token"] != "synthetic-recovered" {
			t.Fatal("recovery must retain file identity and return the renewed token")
		}
	}
	if executor.calls != 1 {
		t.Fatalf("stale callers must reuse owner renewal; got %d refreshes", executor.calls)
	}
	if selected.Metadata["access_token"] != "synthetic-old" {
		t.Fatal("recovery mutated the caller's selected snapshot")
	}
}

func TestQuotaRecoveryRejectsExplicitNonFileSources(t *testing.T) {
	for _, source := range []string{
		coreauth.AuthSourceMemory, coreauth.AuthSourceConfig, coreauth.AuthSourceGit,
		coreauth.AuthSourceObjectStore, coreauth.AuthSourcePostgres, "runtime_only",
	} {
		t.Run(source, func(t *testing.T) {
			a := synthesizedQuotaAuth(t)
			// A legacy filename and loader path cannot override an explicit owner.
			a.FileName = "legacy-present.json"
			if source == "runtime_only" {
				a.Attributes[coreauth.AttributeRuntimeOnly] = "true"
			} else {
				a.Attributes[coreauth.AttributeSourceBackend] = source
			}
			m := coreauth.NewManager(nil, nil, nil)
			executor := &fileQuotaRecoveryExecutor{}
			m.RegisterExecutor(executor)
			selected, err := m.Register(context.Background(), a)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := m.RecoverQuotaCredential(context.Background(), selected); err == nil {
				t.Fatal("non-file source must not use file-owned quota recovery")
			}
			if executor.calls != 0 {
				t.Fatal("ineligible source reached the credential owner")
			}
		})
	}
}
