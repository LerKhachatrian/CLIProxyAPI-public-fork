package codexoauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type staticAuthCatalog struct {
	auths []*coreauth.Auth
}

func (c *staticAuthCatalog) HasEnabledProviderAccount(provider, accountID string) bool {
	if c == nil {
		return false
	}
	for _, auth := range c.auths {
		if auth == nil || auth.Disabled || !strings.EqualFold(strings.TrimSpace(auth.Provider), provider) {
			continue
		}
		candidate, _ := auth.Metadata["account_id"].(string)
		if strings.TrimSpace(candidate) == accountID {
			return true
		}
	}
	return false
}

func allowedCatalog(accountID string) *staticAuthCatalog {
	return &staticAuthCatalog{auths: []*coreauth.Auth{{
		Provider: "codex",
		Metadata: map[string]any{"account_id": accountID},
	}}}
}

func authRequest(remoteAddr, token, accountID string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/v1/models", nil)
	req.RemoteAddr = remoteAddr
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("ChatGPT-Account-ID", accountID)
	req.Header.Set("version", "0.151.0")
	return req
}

func TestProviderAuthenticatesAllowedLoopbackAccountAndCachesOnlyHash(t *testing.T) {
	var validations atomic.Int32
	validator := tokenValidatorFunc(func(_ context.Context, token, accountID, clientVersion string) error {
		validations.Add(1)
		if token != "oauth-token" || accountID != "account-1" || clientVersion != "0.151.0" {
			t.Fatalf("unexpected validation input")
		}
		return nil
	})
	p := newProvider(allowedCatalog("account-1"), validator)

	for range 2 {
		result, authErr := p.Authenticate(context.Background(), authRequest("127.0.0.1:42000", "oauth-token", "account-1"))
		if authErr != nil {
			t.Fatalf("Authenticate() error = %v", authErr)
		}
		if result.Provider != providerID || strings.Contains(result.Principal, "oauth-token") || strings.Contains(result.Principal, "account-1") {
			t.Fatalf("unsafe or unexpected result: %+v", result)
		}
		for key, value := range result.Metadata {
			if strings.Contains(key+value, "oauth-token") || strings.Contains(key+value, "account-1") {
				t.Fatalf("metadata leaked credential identity: %q=%q", key, value)
			}
		}
	}
	if got := validations.Load(); got != 1 {
		t.Fatalf("validations = %d, want 1", got)
	}
	if len(p.cache.entries) != 1 {
		t.Fatalf("cache entries = %d, want 1", len(p.cache.entries))
	}
}

func TestValidationCacheExpiresAndStaysBounded(t *testing.T) {
	cache := newValidationCache()
	now := time.Unix(1_800_000_000, 0)
	cache.now = func() time.Time { return now }
	for index := 0; index < validationCacheMaxEntries+1; index++ {
		var key [32]byte
		key[0] = byte(index)
		key[1] = byte(index >> 8)
		cache.add(key)
	}
	if got := len(cache.entries); got != validationCacheMaxEntries {
		t.Fatalf("cache entries = %d, want %d", got, validationCacheMaxEntries)
	}

	now = now.Add(validationCacheTTL + time.Second)
	cache.add([32]byte{0xff, 0xff, 0xff})
	if got := len(cache.entries); got != 1 {
		t.Fatalf("cache entries after expiry = %d, want 1", got)
	}
}

func TestProviderFailsClosedBeforeValidation(t *testing.T) {
	var validations atomic.Int32
	validator := tokenValidatorFunc(func(context.Context, string, string, string) error {
		validations.Add(1)
		return nil
	})

	tests := []struct {
		name      string
		catalog   *staticAuthCatalog
		remote    string
		token     string
		accountID string
		wantCode  sdkaccess.AuthErrorCode
	}{
		{name: "non-loopback", catalog: allowedCatalog("account-1"), remote: "192.0.2.10:42000", token: "oauth-token", accountID: "account-1", wantCode: sdkaccess.AuthErrorCodeInvalidCredential},
		{name: "account absent from pool", catalog: allowedCatalog("account-2"), remote: "127.0.0.1:42000", token: "oauth-token", accountID: "account-1", wantCode: sdkaccess.AuthErrorCodeInvalidCredential},
		{name: "missing account", catalog: allowedCatalog("account-1"), remote: "127.0.0.1:42000", token: "oauth-token", accountID: "", wantCode: sdkaccess.AuthErrorCodeNotHandled},
		{name: "non-bearer", catalog: allowedCatalog("account-1"), remote: "127.0.0.1:42000", token: "", accountID: "account-1", wantCode: sdkaccess.AuthErrorCodeNotHandled},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p := newProvider(test.catalog, validator)
			req := authRequest(test.remote, test.token, test.accountID)
			if test.token == "" {
				req.Header.Set("Authorization", "Basic not-supported")
			}
			if test.accountID == "" {
				req.Header.Del("ChatGPT-Account-ID")
			}
			_, authErr := p.Authenticate(context.Background(), req)
			if !sdkaccess.IsAuthErrorCode(authErr, test.wantCode) {
				t.Fatalf("Authenticate() error = %#v, want %s", authErr, test.wantCode)
			}
		})
	}
	if got := validations.Load(); got != 0 {
		t.Fatalf("validations = %d, want 0", got)
	}
}

func TestProviderMapsValidationFailures(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode sdkaccess.AuthErrorCode
	}{
		{name: "invalid token", err: errInvalidToken, wantCode: sdkaccess.AuthErrorCodeInvalidCredential},
		{name: "validator unavailable", err: errors.New("temporary validator failure"), wantCode: sdkaccess.AuthErrorCodeInternal},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p := newProvider(allowedCatalog("account-1"), tokenValidatorFunc(func(context.Context, string, string, string) error {
				return test.err
			}))
			_, authErr := p.Authenticate(context.Background(), authRequest("127.0.0.1:42000", "oauth-token", "account-1"))
			if !sdkaccess.IsAuthErrorCode(authErr, test.wantCode) {
				t.Fatalf("Authenticate() error = %#v, want %s", authErr, test.wantCode)
			}
		})
	}
}

func TestProviderDeduplicatesConcurrentValidation(t *testing.T) {
	var validations atomic.Int32
	validator := tokenValidatorFunc(func(context.Context, string, string, string) error {
		validations.Add(1)
		time.Sleep(25 * time.Millisecond)
		return nil
	})
	p := newProvider(allowedCatalog("account-1"), validator)

	const requestCount = 24
	var wg sync.WaitGroup
	errorsCh := make(chan error, requestCount)
	for range requestCount {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, authErr := p.Authenticate(context.Background(), authRequest("[::1]:42000", "oauth-token", "account-1"))
			if authErr != nil {
				errorsCh <- authErr
			}
		}()
	}
	wg.Wait()
	close(errorsCh)
	for err := range errorsCh {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if got := validations.Load(); got != 1 {
		t.Fatalf("validations = %d, want 1", got)
	}
}

func TestProviderBoundsConcurrentUniqueTokenValidation(t *testing.T) {
	var active atomic.Int32
	var maximum atomic.Int32
	validator := tokenValidatorFunc(func(context.Context, string, string, string) error {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			observed := maximum.Load()
			if current <= observed || maximum.CompareAndSwap(observed, current) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		return nil
	})
	p := newProvider(allowedCatalog("account-1"), validator)

	const requestCount = validationMaxConcurrent * 3
	var wg sync.WaitGroup
	errorsCh := make(chan error, requestCount)
	for index := range requestCount {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token := fmt.Sprintf("oauth-token-%d", index)
			_, authErr := p.Authenticate(context.Background(), authRequest("127.0.0.1:42000", token, "account-1"))
			if authErr != nil {
				errorsCh <- authErr
			}
		}()
	}
	wg.Wait()
	close(errorsCh)
	for err := range errorsCh {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if got := maximum.Load(); got > validationMaxConcurrent {
		t.Fatalf("maximum concurrent validations = %d, want <= %d", got, validationMaxConcurrent)
	}
}

func TestProviderRechecksPoolAuthorizationBeforeUsingCache(t *testing.T) {
	catalog := allowedCatalog("account-1")
	var validations atomic.Int32
	p := newProvider(catalog, tokenValidatorFunc(func(context.Context, string, string, string) error {
		validations.Add(1)
		return nil
	}))
	req := authRequest("127.0.0.1:42000", "oauth-token", "account-1")
	if _, authErr := p.Authenticate(context.Background(), req); authErr != nil {
		t.Fatalf("first Authenticate() error = %v", authErr)
	}
	catalog.auths[0].Disabled = true
	if _, authErr := p.Authenticate(context.Background(), req); !sdkaccess.IsAuthErrorCode(authErr, sdkaccess.AuthErrorCodeInvalidCredential) {
		t.Fatalf("disabled account error = %#v", authErr)
	}
	if got := validations.Load(); got != 1 {
		t.Fatalf("validations = %d, want 1", got)
	}
}

func TestOpenAITokenValidatorUsesOnlyExpectedCredentialHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer oauth-token" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("ChatGPT-Account-ID"); got != "account-1" {
			t.Errorf("ChatGPT-Account-ID = %q", got)
		}
		if got := r.Header.Get("Originator"); got != "codex_cli_rs" {
			t.Errorf("Originator = %q", got)
		}
		if got := r.URL.Query().Get("client_version"); got != "0.151.0" {
			t.Errorf("client_version = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer server.Close()

	validator := &openAITokenValidator{client: server.Client(), modelsURL: server.URL}
	if err := validator.Validate(context.Background(), "oauth-token", "account-1", "0.151.0"); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestOpenAITokenValidatorClassifiesStatus(t *testing.T) {
	tests := []struct {
		status      int
		wantInvalid bool
	}{
		{status: http.StatusUnauthorized, wantInvalid: true},
		{status: http.StatusForbidden, wantInvalid: true},
		{status: http.StatusBadGateway, wantInvalid: false},
	}
	for _, test := range tests {
		t.Run(http.StatusText(test.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
			}))
			defer server.Close()
			validator := &openAITokenValidator{client: server.Client(), modelsURL: server.URL}
			err := validator.Validate(context.Background(), "oauth-token", "account-1", "")
			if test.wantInvalid != errors.Is(err, errInvalidToken) {
				t.Fatalf("Validate() error = %v, wantInvalid = %v", err, test.wantInvalid)
			}
		})
	}
}

func TestRegisterRequiresExplicitLoopbackAndEnabledCatalog(t *testing.T) {
	sdkaccess.UnregisterProvider(sdkaccess.AccessProviderTypeCodexClientOAuth)
	t.Cleanup(func() {
		sdkaccess.UnregisterProvider(sdkaccess.AccessProviderTypeCodexClientOAuth)
	})

	nonLoopback := &config.Config{Host: "0.0.0.0"}
	nonLoopback.Codex.ClientOAuthAccess.Enabled = true
	if err := Register(nonLoopback, allowedCatalog("account-1")); err == nil {
		t.Fatal("Register() error = nil for non-loopback host")
	}
	for _, provider := range sdkaccess.RegisteredProviders() {
		if provider.Identifier() == providerID {
			t.Fatal("provider registered after non-loopback failure")
		}
	}

	loopback := &config.Config{Host: "127.0.0.1"}
	loopback.Codex.ClientOAuthAccess.Enabled = true
	if err := Register(loopback, allowedCatalog("account-1")); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	found := false
	for _, provider := range sdkaccess.RegisteredProviders() {
		if provider.Identifier() == providerID {
			found = true
		}
	}
	if !found {
		t.Fatal("registered provider not found")
	}

	loopback.Codex.ClientOAuthAccess.Enabled = false
	if err := Register(loopback, allowedCatalog("account-1")); err != nil {
		t.Fatalf("disable Register() error = %v", err)
	}
	for _, provider := range sdkaccess.RegisteredProviders() {
		if provider.Identifier() == providerID {
			t.Fatal("provider remained registered after disable")
		}
	}
}
