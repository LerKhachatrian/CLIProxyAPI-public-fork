package auth

import "testing"

func TestHasEnabledProviderAccount(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.auths = map[string]*Auth{
		"enabled": {
			Provider: "codex",
			Metadata: map[string]any{"account_id": "account-1", "access_token": "must-not-be-cloned"},
		},
		"disabled-flag": {
			Provider: "codex",
			Disabled: true,
			Metadata: map[string]any{"account_id": "account-2"},
		},
		"disabled-status": {
			Provider: "codex",
			Status:   StatusDisabled,
			Metadata: map[string]any{"account_id": "account-3"},
		},
		"other-provider": {
			Provider: "claude",
			Metadata: map[string]any{"account_id": "account-4"},
		},
	}

	tests := []struct {
		name      string
		provider  string
		accountID string
		want      bool
	}{
		{name: "enabled exact account", provider: "CODEX", accountID: "account-1", want: true},
		{name: "disabled flag", provider: "codex", accountID: "account-2"},
		{name: "disabled status", provider: "codex", accountID: "account-3"},
		{name: "wrong provider", provider: "codex", accountID: "account-4"},
		{name: "missing account", provider: "codex", accountID: "account-missing"},
		{name: "empty provider", accountID: "account-1"},
		{name: "empty account", provider: "codex"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := manager.HasEnabledProviderAccount(test.provider, test.accountID); got != test.want {
				t.Fatalf("HasEnabledProviderAccount() = %v, want %v", got, test.want)
			}
		})
	}
}
