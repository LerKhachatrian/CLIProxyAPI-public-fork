package auth

import "strings"

// HasEnabledProviderAccount reports whether an enabled runtime auth record has
// the exact provider account ID. It performs the lookup under the manager read
// lock without cloning credential metadata.
func (m *Manager) HasEnabledProviderAccount(provider, accountID string) bool {
	if m == nil {
		return false
	}
	provider = strings.TrimSpace(provider)
	accountID = strings.TrimSpace(accountID)
	if provider == "" || accountID == "" {
		return false
	}

	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, auth := range m.auths {
		if auth == nil || auth.Disabled || auth.Status == StatusDisabled || !strings.EqualFold(strings.TrimSpace(auth.Provider), provider) {
			continue
		}
		candidate, _ := auth.Metadata["account_id"].(string)
		if strings.TrimSpace(candidate) == accountID {
			return true
		}
	}
	return false
}
