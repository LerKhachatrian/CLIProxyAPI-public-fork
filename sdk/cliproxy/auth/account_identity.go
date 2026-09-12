package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	codexauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codex"
)

// CodexAccountIdentity is an opaque, stable account/plan identity. Registered
// snapshots precompute it at the auth lifecycle boundary, so selection never
// parses credentials. The encoding preserves the quota monitor's v1 cache keys.
func (a *Auth) CodexAccountIdentity() string {
	if a == nil || !strings.EqualFold(strings.TrimSpace(a.Provider), "codex") {
		return ""
	}
	if a.codexAccountIdentity != "" {
		return a.codexAccountIdentity
	}
	return codexAccountIdentity(a)
}

func codexAccountIdentity(a *Auth) string {
	if a == nil || !strings.EqualFold(strings.TrimSpace(a.Provider), "codex") {
		return ""
	}
	email := ""
	if value, ok := a.Metadata["email"].(string); ok {
		email = strings.TrimSpace(value)
	} else if email = strings.TrimSpace(a.Attributes["email"]); email == "" {
		email = strings.TrimSpace(a.Attributes["account_email"])
	}
	var accountClaim, planClaim any
	if raw, ok := a.Metadata["id_token"].(string); ok && len(raw) <= 64*1024 {
		if claims, err := codexauth.ParseJWTToken(strings.TrimSpace(raw)); err == nil && claims != nil {
			if value := strings.TrimSpace(claims.CodexAuthInfo.ChatgptAccountID); value != "" {
				accountClaim = value
			}
			if value := strings.TrimSpace(claims.CodexAuthInfo.ChatgptPlanType); value != "" {
				planClaim = value
			}
		}
	}
	identity, _ := json.Marshal([]any{a.ID, a.Index, a.Provider, email,
		a.Metadata["account_id"], a.Metadata["plan_type"], accountClaim, planClaim})
	digest := sha256.Sum256(identity)
	return hex.EncodeToString(digest[:])
}
