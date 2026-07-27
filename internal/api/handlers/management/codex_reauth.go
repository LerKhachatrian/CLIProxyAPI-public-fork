package management

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codex"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

var (
	errCodexReauthRegistryUnavailable = errors.New("codex reauth registry unavailable")
	errCodexReauthTargetNotFound      = errors.New("codex reauth target not found")
	errCodexReauthTargetAmbiguous     = errors.New("codex reauth target is ambiguous")
	errCodexReauthTargetUnsupported   = errors.New("codex reauth target is unsupported")
	errCodexReauthIdentityMismatch    = errors.New("codex reauth identity mismatch")
	errCodexReauthIdentityUnknown     = errors.New("codex reauth identity could not be verified")
)

type codexReauthTarget struct {
	authIndex       string
	id              string
	fileName        string
	path            string
	expectedAccount string
	expectedEmail   string
	prefix          string
	proxyURL        string
	label           string
	disabled        bool
	createdAt       time.Time
	allowedMetadata map[string]any
	allowedAttrs    map[string]string
}

func (h *Handler) resolveCodexReauthTarget(authIndex string) (*codexReauthTarget, error) {
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" {
		return nil, fmt.Errorf("%w: empty auth index", errCodexReauthTargetNotFound)
	}
	if h == nil {
		return nil, errCodexReauthRegistryUnavailable
	}

	h.mu.Lock()
	manager := h.authManager
	cfg := h.cfg
	h.mu.Unlock()
	if manager == nil {
		return nil, errCodexReauthRegistryUnavailable
	}

	var match *coreauth.Auth
	for _, auth := range manager.List() {
		if auth == nil {
			continue
		}
		if auth.EnsureIndex() != authIndex {
			continue
		}
		if match != nil {
			return nil, errCodexReauthTargetAmbiguous
		}
		match = auth
	}
	if match == nil {
		return nil, errCodexReauthTargetNotFound
	}
	if !strings.EqualFold(strings.TrimSpace(match.Provider), "codex") ||
		match.AuthSourceKind() != coreauth.AuthSourceFile ||
		match.AuthKind() == coreauth.AuthKindAPIKey ||
		isRuntimeOnlyAuth(match) {
		return nil, errCodexReauthTargetUnsupported
	}

	path := strings.TrimSpace(authAttribute(match, coreauth.AttributePath))
	if path == "" {
		path = strings.TrimSpace(match.FileName)
		if path != "" && !filepath.IsAbs(path) && cfg != nil {
			path = filepath.Join(cfg.AuthDir, path)
		}
	}
	path = cleanAuthFilePath(path)
	if path == "" || !strings.EqualFold(filepath.Ext(path), ".json") {
		return nil, errCodexReauthTargetUnsupported
	}
	if info, errStat := os.Stat(path); errStat != nil || info.IsDir() {
		return nil, errCodexReauthTargetUnsupported
	}

	return &codexReauthTarget{
		authIndex:       authIndex,
		id:              strings.TrimSpace(match.ID),
		fileName:        strings.TrimSpace(match.FileName),
		path:            path,
		expectedAccount: codexReauthMetadataString(match.Metadata, "account_id"),
		expectedEmail:   authEmail(match),
		prefix:          strings.TrimSpace(match.Prefix),
		proxyURL:        strings.TrimSpace(match.ProxyURL),
		label:           strings.TrimSpace(match.Label),
		disabled:        match.Disabled || match.Status == coreauth.StatusDisabled,
		createdAt:       match.CreatedAt,
		allowedMetadata: codexReauthAllowedMetadata(match),
		allowedAttrs:    codexReauthAllowedAttributes(match, path),
	}, nil
}

func writeCodexReauthTargetError(c *gin.Context, err error) {
	status := http.StatusConflict
	message := "Selected account is not a file-backed Codex OAuth account"
	switch {
	case errors.Is(err, errCodexReauthRegistryUnavailable):
		status = http.StatusServiceUnavailable
		message = "Codex account registry is unavailable"
	case errors.Is(err, errCodexReauthTargetNotFound):
		status = http.StatusNotFound
		message = "Codex account was not found"
	case errors.Is(err, errCodexReauthTargetAmbiguous):
		message = "Codex account target is ambiguous"
	}
	c.JSON(status, gin.H{"status": "error", "error": message})
}

func codexReauthAllowedMetadata(auth *coreauth.Auth) map[string]any {
	metadata := make(map[string]any)
	if auth == nil {
		return metadata
	}

	prefix := strings.TrimSpace(auth.Prefix)
	if prefix == "" {
		prefix = codexReauthMetadataString(auth.Metadata, "prefix")
	}
	if prefix != "" {
		metadata["prefix"] = prefix
	}
	proxyURL := strings.TrimSpace(auth.ProxyURL)
	if proxyURL == "" {
		proxyURL = codexReauthMetadataString(auth.Metadata, "proxy_url")
	}
	if proxyURL != "" {
		metadata["proxy_url"] = proxyURL
	}
	if label := strings.TrimSpace(auth.Label); label != "" {
		metadata["label"] = label
	}
	metadata["disabled"] = auth.Disabled || auth.Status == coreauth.StatusDisabled

	if priority, ok := codexReauthPriority(auth); ok {
		metadata["priority"] = priority
	}
	if note := codexReauthNote(auth); note != "" {
		metadata["note"] = note
	}
	if websockets, ok := authWebsocketsValue(auth); ok {
		metadata["websockets"] = websockets
	}
	if headers := codexReauthHeaders(auth); len(headers) > 0 {
		values := make(map[string]any, len(headers))
		for name, value := range headers {
			values[name] = value
		}
		metadata["headers"] = values
	}
	return metadata
}

func codexReauthAllowedAttributes(auth *coreauth.Auth, path string) map[string]string {
	attributes := map[string]string{
		coreauth.AttributePath:          path,
		coreauth.AttributeSource:        path,
		coreauth.AttributeSourceBackend: coreauth.AuthSourceFile,
	}
	if auth == nil {
		return attributes
	}
	for _, key := range []string{
		coreauth.AttributeAuthIndexSeed,
		"priority",
		"note",
		"websockets",
	} {
		if value := strings.TrimSpace(auth.Attributes[key]); value != "" {
			attributes[key] = value
		}
	}
	for key, value := range auth.Attributes {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(key)), "header:") && strings.TrimSpace(value) != "" {
			attributes[key] = value
		}
	}
	return attributes
}

func codexReauthPriority(auth *coreauth.Auth) (int, bool) {
	if auth == nil {
		return 0, false
	}
	if raw := strings.TrimSpace(authAttribute(auth, "priority")); raw != "" {
		value, errParse := strconv.Atoi(raw)
		if errParse == nil {
			return value, true
		}
	}
	if auth.Metadata != nil {
		return authFileIntValue(auth.Metadata["priority"])
	}
	return 0, false
}

func codexReauthNote(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if note := strings.TrimSpace(authAttribute(auth, "note")); note != "" {
		return note
	}
	return codexReauthMetadataString(auth.Metadata, "note")
}

func codexReauthHeaders(auth *coreauth.Auth) map[string]string {
	if auth == nil {
		return nil
	}
	headers := coreauth.ExtractCustomHeadersFromMetadata(auth.Metadata)
	if len(headers) > 0 {
		return headers
	}
	headers = make(map[string]string)
	for key, value := range auth.Attributes {
		trimmedKey := strings.TrimSpace(key)
		if !strings.HasPrefix(strings.ToLower(trimmedKey), "header:") {
			continue
		}
		name := strings.TrimSpace(trimmedKey[len("header:"):])
		if name == "" || strings.TrimSpace(value) == "" {
			continue
		}
		headers[name] = strings.TrimSpace(value)
	}
	if len(headers) == 0 {
		return nil
	}
	return headers
}

func codexReauthMetadataString(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	value, _ := metadata[key].(string)
	return strings.TrimSpace(value)
}

func (target *codexReauthTarget) verifyIdentity(storage *codex.CodexTokenStorage) error {
	if target == nil || storage == nil {
		return errCodexReauthIdentityUnknown
	}
	actualAccount := strings.TrimSpace(storage.AccountID)
	actualEmail := strings.TrimSpace(storage.Email)
	if target.expectedAccount != "" && actualAccount != "" {
		if target.expectedAccount == actualAccount {
			return nil
		}
		return errCodexReauthIdentityMismatch
	}
	if target.expectedEmail != "" && actualEmail != "" {
		if strings.EqualFold(target.expectedEmail, actualEmail) {
			return nil
		}
		return errCodexReauthIdentityMismatch
	}
	return errCodexReauthIdentityUnknown
}

func (target *codexReauthTarget) buildRecord(storage *codex.CodexTokenStorage) *coreauth.Auth {
	metadata := make(map[string]any, len(target.allowedMetadata)+2)
	for key, value := range target.allowedMetadata {
		metadata[key] = value
	}
	metadata["email"] = strings.TrimSpace(storage.Email)
	metadata["account_id"] = strings.TrimSpace(storage.AccountID)

	attributes := make(map[string]string, len(target.allowedAttrs))
	for key, value := range target.allowedAttrs {
		attributes[key] = value
	}
	status := coreauth.StatusActive
	if target.disabled {
		status = coreauth.StatusDisabled
	}
	createdAt := target.createdAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	return &coreauth.Auth{
		ID:         target.id,
		Provider:   "codex",
		Prefix:     target.prefix,
		FileName:   target.fileName,
		Storage:    storage,
		Label:      target.label,
		Status:     status,
		Disabled:   target.disabled,
		ProxyURL:   target.proxyURL,
		Attributes: attributes,
		Metadata:   metadata,
		CreatedAt:  createdAt,
		UpdatedAt:  time.Now(),
	}
}

func (h *Handler) updateCodexReauthRuntime(ctx context.Context, record *coreauth.Auth) error {
	if h == nil || record == nil {
		return errCodexReauthRegistryUnavailable
	}
	h.mu.Lock()
	manager := h.authManager
	h.mu.Unlock()
	if manager == nil {
		return errCodexReauthRegistryUnavailable
	}
	updated, errUpdate := manager.Update(coreauth.WithSkipPersist(ctx), record)
	if errUpdate != nil {
		return errUpdate
	}
	if updated == nil {
		return errCodexReauthTargetNotFound
	}
	return nil
}
