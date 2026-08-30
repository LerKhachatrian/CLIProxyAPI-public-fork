// Package codexoauth authenticates official Codex clients to a loopback-only
// CLIProxy listener with their existing ChatGPT OAuth credentials.
package codexoauth

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
)

const (
	providerID                = "codex-client-oauth"
	chatGPTModelsURL          = "https://chatgpt.com/backend-api/codex/models"
	validationTimeout         = 10 * time.Second
	validationCacheTTL        = time.Minute
	validationCacheMaxEntries = 256
	validationMaxConcurrent   = 8
)

var errInvalidToken = errors.New("OpenAI rejected the Codex client credential")

type authCatalog interface {
	HasEnabledProviderAccount(string, string) bool
}

type tokenValidator interface {
	Validate(context.Context, string, string, string) error
}

type tokenValidatorFunc func(context.Context, string, string, string) error

func (f tokenValidatorFunc) Validate(ctx context.Context, token, accountID, clientVersion string) error {
	return f(ctx, token, accountID, clientVersion)
}

type validationCache struct {
	mu      sync.Mutex
	entries map[[32]byte]time.Time
	now     func() time.Time
}

func newValidationCache() *validationCache {
	return &validationCache{
		entries: make(map[[32]byte]time.Time),
		now:     time.Now,
	}
}

func (c *validationCache) valid(key [32]byte) bool {
	if c == nil {
		return false
	}
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	expiresAt, ok := c.entries[key]
	if !ok {
		return false
	}
	if !expiresAt.After(now) {
		delete(c.entries, key)
		return false
	}
	return true
}

func (c *validationCache) add(key [32]byte) {
	if c == nil {
		return
	}
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for cachedKey, expiresAt := range c.entries {
		if !expiresAt.After(now) {
			delete(c.entries, cachedKey)
		}
	}
	if len(c.entries) >= validationCacheMaxEntries {
		var oldestKey [32]byte
		var oldestExpiry time.Time
		for cachedKey, expiresAt := range c.entries {
			if oldestExpiry.IsZero() || expiresAt.Before(oldestExpiry) {
				oldestKey = cachedKey
				oldestExpiry = expiresAt
			}
		}
		delete(c.entries, oldestKey)
	}
	c.entries[key] = now.Add(validationCacheTTL)
}

type provider struct {
	auths     authCatalog
	validator tokenValidator
	cache     *validationCache
	requests  singleflight.Group
	validate  chan struct{}
}

func newProvider(auths authCatalog, validator tokenValidator) *provider {
	if validator == nil {
		validator = &openAITokenValidator{
			client:    &http.Client{Timeout: validationTimeout},
			modelsURL: chatGPTModelsURL,
		}
	}
	return &provider{
		auths:     auths,
		validator: validator,
		cache:     newValidationCache(),
		validate:  make(chan struct{}, validationMaxConcurrent),
	}
}

func (p *provider) Identifier() string {
	return providerID
}

func (p *provider) Authenticate(ctx context.Context, r *http.Request) (*sdkaccess.Result, *sdkaccess.AuthError) {
	if p == nil || r == nil {
		return nil, sdkaccess.NewNotHandledError()
	}
	token, ok := bearerToken(r.Header.Get("Authorization"))
	accountID := strings.TrimSpace(r.Header.Get("ChatGPT-Account-ID"))
	if !ok || accountID == "" {
		return nil, sdkaccess.NewNotHandledError()
	}
	if !requestIsLoopback(r) || !p.accountAllowed(accountID) {
		return nil, sdkaccess.NewInvalidCredentialError()
	}

	cacheKey := sha256.Sum256([]byte(token + "\x00" + accountID))
	if !p.cache.valid(cacheKey) {
		flightKey := fmt.Sprintf("%x", cacheKey)
		_, errValidate, _ := p.requests.Do(flightKey, func() (any, error) {
			if p.cache.valid(cacheKey) {
				return nil, nil
			}
			if err := p.validateToken(ctx, token, accountID, safeClientVersion(r)); err != nil {
				return nil, err
			}
			p.cache.add(cacheKey)
			return nil, nil
		})
		if errValidate != nil {
			if errors.Is(errValidate, errInvalidToken) {
				return nil, sdkaccess.NewInvalidCredentialError()
			}
			return nil, sdkaccess.NewInternalAuthError("Codex client credential validation failed", errValidate)
		}
	}

	principalHash := sha256.Sum256([]byte(accountID))
	return &sdkaccess.Result{
		Provider:  providerID,
		Principal: fmt.Sprintf("codex-account-%x", principalHash[:8]),
		Metadata: map[string]string{
			"source": "chatgpt-oauth",
		},
	}, nil
}

func (p *provider) validateToken(ctx context.Context, token, accountID, clientVersion string) error {
	if p == nil || p.validator == nil || p.validate == nil {
		return fmt.Errorf("Codex client credential validator is unavailable")
	}
	select {
	case p.validate <- struct{}{}:
		defer func() { <-p.validate }()
	case <-ctx.Done():
		return fmt.Errorf("waiting for Codex client credential validation: %w", ctx.Err())
	}
	return p.validator.Validate(ctx, token, accountID, clientVersion)
}

func (p *provider) accountAllowed(accountID string) bool {
	if p == nil || p.auths == nil || accountID == "" {
		return false
	}
	return p.auths.HasEnabledProviderAccount("codex", accountID)
}

func bearerToken(header string) (string, bool) {
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || strings.TrimSpace(parts[1]) == "" {
		return "", false
	}
	return strings.TrimSpace(parts[1]), true
}

func requestIsLoopback(r *http.Request) bool {
	if r == nil {
		return false
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		host = strings.Trim(strings.TrimSpace(r.RemoteAddr), "[]")
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func safeClientVersion(r *http.Request) string {
	if r == nil {
		return ""
	}
	value := strings.TrimSpace(r.Header.Get("version"))
	if value == "" || len(value) > 64 {
		return ""
	}
	for _, char := range value {
		if (char < '0' || char > '9') && char != '.' && char != '-' && char != '+' {
			return ""
		}
	}
	return value
}

func loopbackBindHost(host string) bool {
	host = strings.Trim(strings.TrimSpace(host), "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ValidateListenerConfig rejects an enabled client OAuth provider unless the
// server is explicitly bound to loopback.
func ValidateListenerConfig(cfg *config.Config) error {
	if cfg == nil || !cfg.Codex.ClientOAuthAccess.Enabled {
		return nil
	}
	if !loopbackBindHost(cfg.Host) {
		return fmt.Errorf("codex.client-oauth-access requires an explicit loopback host")
	}
	return nil
}

// Register installs or removes the Codex client OAuth access provider.
// Enabling it on a non-loopback listener fails closed.
func Register(cfg *config.Config, auths authCatalog) error {
	if cfg == nil || !cfg.Codex.ClientOAuthAccess.Enabled {
		sdkaccess.UnregisterProvider(sdkaccess.AccessProviderTypeCodexClientOAuth)
		return nil
	}
	if errConfig := ValidateListenerConfig(cfg); errConfig != nil {
		return errConfig
	}
	if auths == nil {
		return fmt.Errorf("codex.client-oauth-access requires the runtime Codex auth catalog")
	}
	sdkaccess.RegisterProvider(
		sdkaccess.AccessProviderTypeCodexClientOAuth,
		newProvider(auths, nil),
	)
	return nil
}

type openAITokenValidator struct {
	client    *http.Client
	modelsURL string
}

func (v *openAITokenValidator) Validate(ctx context.Context, token, accountID, clientVersion string) error {
	if v == nil || v.client == nil {
		return fmt.Errorf("OpenAI validation client is unavailable")
	}
	modelsEndpoint := strings.TrimSpace(v.modelsURL)
	if modelsEndpoint == "" {
		modelsEndpoint = chatGPTModelsURL
	}
	modelsURL, errURL := url.Parse(modelsEndpoint)
	if errURL != nil {
		return fmt.Errorf("constructing OpenAI validation URL: %w", errURL)
	}
	if clientVersion != "" {
		query := modelsURL.Query()
		query.Set("client_version", clientVersion)
		modelsURL.RawQuery = query.Encode()
	}
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL.String(), nil)
	if errRequest != nil {
		return fmt.Errorf("constructing OpenAI validation request: %w", errRequest)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("ChatGPT-Account-ID", accountID)
	req.Header.Set("Originator", "codex_cli_rs")
	req.Header.Set("User-Agent", "codex_cli_rs/cliproxy-client-oauth")

	resp, errDo := v.client.Do(req)
	if errDo != nil {
		return fmt.Errorf("validating with OpenAI: %w", errDo)
	}
	_, _ = io.CopyN(io.Discard, resp.Body, 4096)
	_ = resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return errInvalidToken
	}
	return fmt.Errorf("OpenAI validation returned HTTP %d", resp.StatusCode)
}
