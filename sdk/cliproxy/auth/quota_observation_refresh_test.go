package auth

import (
	"context"
	"testing"
	"time"
)

type quotaCredentialExecutor struct {
	ProviderExecutor
	calls int
}

func (e *quotaCredentialExecutor) Identifier() string { return "codex" }
func (e *quotaCredentialExecutor) Refresh(_ context.Context, a *Auth) (*Auth, error) {
	e.calls++
	a.Metadata["access_token"] = "synthetic-new"
	a.Metadata["plan_type"] = "pro"
	a.Metadata["expired"] = time.Now().Add(time.Hour).Format(time.RFC3339)
	return a, nil
}

func TestQuotaCredentialRecoveryUsesOwnerAndReusesNewerToken(t *testing.T) {
	m := NewManager(nil, nil, nil)
	exec := &quotaCredentialExecutor{}
	m.RegisterExecutor(exec)
	a, err := m.Register(context.Background(), &Auth{ID: "quota-test", FileName: "synthetic.json", Provider: "codex",
		Metadata: map[string]any{"access_token": "synthetic-old", "refresh_token": "synthetic-refresh", "account_id": "one", "plan_type": "prolite"}})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		updated, err := m.RecoverQuotaCredential(context.Background(), a)
		if err != nil || updated == nil || authAccessToken(updated) != "synthetic-new" || updated.Metadata["plan_type"] != "pro" {
			t.Fatal("owner recovery failed", err)
		}
	}
	if exec.calls != 1 || authAccessToken(a) != "synthetic-old" {
		t.Fatal("duplicate refresh or mutated caller")
	}
}

func TestQuotaCredentialRecoveryDoesNotWaitOrCrossOwnerGuards(t *testing.T) {
	for _, mode := range []string{"busy", "disabled", "replaced", "cooldown", "cancelled", "external"} {
		t.Run(mode, func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			exec := &quotaCredentialExecutor{}
			m.RegisterExecutor(exec)
			a, _ := m.Register(context.Background(), &Auth{ID: "quota-guard", FileName: "synthetic.json", Provider: "codex",
				Metadata: map[string]any{"access_token": "synthetic-old", "refresh_token": "synthetic-refresh", "account_id": "one"}})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "busy":
				lock := &authRefreshLock{}
				lock.mu.Lock()
				defer lock.mu.Unlock()
				m.refreshLocks.Store(a.ID, lock)
			case "disabled":
				m.auths[a.ID].Disabled = true
			case "replaced":
				a.RegistrationEpoch++
			case "cooldown":
				m.auths[a.ID].NextRefreshAfter = time.Now().Add(time.Minute)
			case "cancelled":
				cancel()
			case "external":
				a.FileName = ""
			}
			finished := make(chan error, 1)
			go func() { _, err := m.RecoverQuotaCredential(ctx, a); finished <- err }()
			select {
			case err := <-finished:
				if err == nil || exec.calls != 0 {
					t.Fatal("owner guard bypassed")
				}
			case <-time.After(time.Second):
				t.Fatal("quota recovery waited behind another owner")
			}
		})
	}
}
