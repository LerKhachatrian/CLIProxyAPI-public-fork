// Command codex_client_oauth_staging_proxy is a temporary loopback-only E2E
// harness. It validates official Codex ChatGPT OAuth credentials with the real
// access provider, then forwards authorized traffic to an already-running
// CLIProxy using the operator's existing CLIPROXY_API_KEY. It never persists
// bearer tokens or account IDs and is not a production deployment surface.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	codexoauth "github.com/router-for-me/CLIProxyAPI/v7/internal/access/codex_oauth"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

const (
	maxSyntheticAccounts = 16
	maxTierCaptureEvents = 64
	maxTierCaptureBody   = 1 << 20
)

type tierCaptureEvent struct {
	Sequence uint64 `json:"sequence"`
	Tier     string `json:"tier"`
}

type tierCapture struct {
	mu       sync.Mutex
	sequence uint64
	events   []tierCaptureEvent
}

func (c *tierCapture) record(tier string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sequence++
	c.events = append(c.events, tierCaptureEvent{Sequence: c.sequence, Tier: tier})
	if len(c.events) > maxTierCaptureEvents {
		c.events = append([]tierCaptureEvent(nil), c.events[len(c.events)-maxTierCaptureEvents:]...)
	}
}

func (c *tierCapture) snapshot() []tierCaptureEvent {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]tierCaptureEvent(nil), c.events...)
}

func (c *tierCapture) reset() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.sequence = 0
	c.events = nil
	c.mu.Unlock()
}

type replayReadCloser struct {
	io.Reader
	io.Closer
}

type serviceTierEnvelope struct {
	ServiceTier json.RawMessage `json:"service_tier"`
}

type websocketTierEnvelope struct {
	Type        string          `json:"type"`
	ServiceTier json.RawMessage `json:"service_tier"`
}

type boundedCaptureBuffer struct {
	buffer    bytes.Buffer
	truncated bool
}

func (b *boundedCaptureBuffer) Write(value []byte) (int, error) {
	if b == nil {
		return len(value), nil
	}
	remaining := maxTierCaptureBody - b.buffer.Len()
	if remaining > 0 {
		writeLength := min(remaining, len(value))
		_, _ = b.buffer.Write(value[:writeLength])
	}
	if len(value) > remaining {
		b.truncated = true
	}
	return len(value), nil
}

type stagingHandler struct {
	accessManager *sdkaccess.Manager
	authManager   *coreauth.Manager
	proxy         *httputil.ReverseProxy
	targetURL     *url.URL
	proxyKey      string
	tierCapture   *tierCapture
	mu            sync.Mutex
	accounts      map[string]struct{}
}

func (h *stagingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/__staging_health" {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
		return
	}
	if r.URL.Path == "/__staging_tiers" {
		h.serveTierCapture(w, r)
		return
	}
	accountID := strings.TrimSpace(r.Header.Get("ChatGPT-Account-ID"))
	if !safeAccountID(accountID) || !h.allowSyntheticAccount(r.Context(), accountID) {
		http.Error(w, "invalid staging account identity", http.StatusUnauthorized)
		return
	}
	result, authErr := h.accessManager.Authenticate(r.Context(), r)
	if authErr != nil {
		http.Error(w, authErr.Message, authErr.HTTPStatusCode())
		return
	}
	if result == nil || result.Provider != sdkaccess.AccessProviderTypeCodexClientOAuth {
		http.Error(w, "unexpected staging authentication provider", http.StatusUnauthorized)
		return
	}
	if websocket.IsWebSocketUpgrade(r) {
		h.proxyWebSocket(w, r)
		return
	}
	if tier, observed := observeResponseTier(r); observed {
		h.tierCapture.record(tier)
	}
	h.proxy.ServeHTTP(w, r)
}

func (h *stagingHandler) proxyWebSocket(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.targetURL == nil || h.proxyKey == "" {
		http.Error(w, "staging websocket proxy unavailable", http.StatusBadGateway)
		return
	}

	upstreamURL := *h.targetURL
	upstreamURL.Scheme = "ws"
	upstreamURL.Path = joinURLPath(h.targetURL.Path, r.URL.Path)
	upstreamURL.RawQuery = r.URL.RawQuery
	headers := r.Header.Clone()
	for _, name := range []string{
		"Authorization",
		"ChatGPT-Account-ID",
		"Connection",
		"Upgrade",
		"Sec-WebSocket-Key",
		"Sec-WebSocket-Version",
		"Sec-WebSocket-Extensions",
		"Sec-WebSocket-Protocol",
	} {
		headers.Del(name)
	}
	headers.Set("Authorization", "Bearer "+h.proxyKey)

	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		Subprotocols:     websocket.Subprotocols(r),
	}
	upstream, response, errDial := dialer.DialContext(r.Context(), upstreamURL.String(), headers)
	if errDial != nil {
		if response != nil && response.Body != nil {
			_, _ = io.CopyN(io.Discard, response.Body, 4096)
			_ = response.Body.Close()
		}
		http.Error(w, "staging websocket upstream unavailable", http.StatusBadGateway)
		return
	}

	responseHeaders := http.Header{}
	if subprotocol := upstream.Subprotocol(); subprotocol != "" {
		responseHeaders.Set("Sec-WebSocket-Protocol", subprotocol)
	}
	downstream, errUpgrade := (&websocket.Upgrader{
		CheckOrigin: func(*http.Request) bool { return true },
	}).Upgrade(w, r, responseHeaders)
	if errUpgrade != nil {
		_ = upstream.Close()
		return
	}

	errorsCh := make(chan error, 2)
	go func() {
		errorsCh <- relayWebSocketMessages(upstream, downstream, h.tierCapture)
	}()
	go func() {
		errorsCh <- relayWebSocketMessages(downstream, upstream, nil)
	}()
	<-errorsCh
	_ = downstream.Close()
	_ = upstream.Close()
	<-errorsCh
}

func relayWebSocketMessages(destination, source *websocket.Conn, capture *tierCapture) error {
	for {
		messageType, reader, errReader := source.NextReader()
		if errReader != nil {
			return errReader
		}
		writer, errWriter := destination.NextWriter(messageType)
		if errWriter != nil {
			return errWriter
		}

		var observed boundedCaptureBuffer
		copyReader := io.Reader(reader)
		if capture != nil && (messageType == websocket.TextMessage || messageType == websocket.BinaryMessage) {
			copyReader = io.TeeReader(reader, &observed)
		}
		_, errCopy := io.Copy(writer, copyReader)
		errClose := writer.Close()
		if errCopy != nil {
			return errCopy
		}
		if errClose != nil {
			return errClose
		}
		if capture != nil {
			if tier, ok := observeWebSocketTier(observed.buffer.Bytes(), observed.truncated); ok {
				capture.record(tier)
			}
		}
	}
}

func observeWebSocketTier(payload []byte, truncated bool) (string, bool) {
	if truncated {
		if gjson.GetBytes(payload, "type").String() == "response.create" {
			return "unavailable-oversized", true
		}
		return "", false
	}
	var envelope websocketTierEnvelope
	if errJSON := json.Unmarshal(payload, &envelope); errJSON != nil || envelope.Type != "response.create" {
		return "", false
	}
	return classifyServiceTier(envelope.ServiceTier), true
}

func joinURLPath(basePath, requestPath string) string {
	basePath = strings.TrimSuffix(basePath, "/")
	requestPath = "/" + strings.TrimPrefix(requestPath, "/")
	if basePath == "" {
		return requestPath
	}
	return basePath + requestPath
}

func (h *stagingHandler) serveTierCapture(w http.ResponseWriter, r *http.Request) {
	if !requestRemoteIsLoopback(r) {
		http.Error(w, "loopback required", http.StatusForbidden)
		return
	}
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(struct {
			SchemaVersion string             `json:"schema_version"`
			Events        []tierCaptureEvent `json:"events"`
		}{
			SchemaVersion: "cliproxy.codex-client-oauth-staging-tiers.v1",
			Events:        h.tierCapture.snapshot(),
		})
	case http.MethodDelete:
		h.tierCapture.reset()
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func observeResponseTier(r *http.Request) (string, bool) {
	if r == nil || r.Method != http.MethodPost || r.Body == nil {
		return "", false
	}
	path := strings.TrimSuffix(r.URL.Path, "/")
	if path != "/v1/responses" && path != "/responses" {
		return "", false
	}

	originalBody := r.Body
	body, errRead := io.ReadAll(io.LimitReader(originalBody, maxTierCaptureBody+1))
	if len(body) > maxTierCaptureBody || errRead != nil {
		r.Body = &replayReadCloser{
			Reader: io.MultiReader(bytes.NewReader(body), originalBody),
			Closer: originalBody,
		}
		if errRead != nil {
			return "unavailable-read-error", true
		}
		return "unavailable-oversized", true
	}
	_ = originalBody.Close()
	r.Body = io.NopCloser(bytes.NewReader(body))

	var envelope serviceTierEnvelope
	if errJSON := json.Unmarshal(body, &envelope); errJSON != nil {
		return "unavailable-invalid-json", true
	}
	return classifyServiceTier(envelope.ServiceTier), true
}

func classifyServiceTier(raw json.RawMessage) string {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "absent"
	}
	var tier string
	if errTier := json.Unmarshal(raw, &tier); errTier != nil {
		return "other"
	}
	switch strings.ToLower(strings.TrimSpace(tier)) {
	case "fast":
		return "fast"
	case "priority":
		return "priority"
	case "default":
		return "default"
	case "auto":
		return "auto"
	default:
		return "other"
	}
}

func requestRemoteIsLoopback(r *http.Request) bool {
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

func (h *stagingHandler) allowSyntheticAccount(ctx context.Context, accountID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.accounts[accountID]; ok {
		return true
	}
	if len(h.accounts) >= maxSyntheticAccounts {
		return false
	}
	_, err := h.authManager.Register(ctx, &coreauth.Auth{
		Provider:   "codex",
		Attributes: map[string]string{"runtime_only": "true"},
		Metadata:   map[string]any{"account_id": accountID},
	})
	if err != nil {
		return false
	}
	h.accounts[accountID] = struct{}{}
	return true
}

func safeAccountID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '-' && char != '_' {
			return false
		}
	}
	return true
}

func loopbackAddress(value string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(value))
	if err != nil {
		return false
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func run() error {
	listenAddress := flag.String("listen", "127.0.0.1:48318", "loopback staging listen address")
	targetAddress := flag.String("target", "http://127.0.0.1:48317", "loopback CLIProxy target")
	flag.Parse()

	if !loopbackAddress(*listenAddress) {
		return fmt.Errorf("staging listen address must be explicit loopback")
	}
	targetURL, errTarget := url.Parse(*targetAddress)
	if errTarget != nil || targetURL.Scheme != "http" || !loopbackAddress(targetURL.Host) {
		return fmt.Errorf("staging target must be an explicit loopback HTTP URL")
	}
	if strings.TrimSpace(targetURL.Host) == strings.TrimSpace(*listenAddress) {
		return fmt.Errorf("staging target and listener must differ")
	}
	proxyKey := strings.TrimSpace(os.Getenv("CLIPROXY_API_KEY"))
	if proxyKey == "" {
		return fmt.Errorf("CLIPROXY_API_KEY is required")
	}

	authManager := coreauth.NewManager(nil, nil, nil)
	cfg := &config.Config{Host: "127.0.0.1"}
	cfg.Codex.ClientOAuthAccess.Enabled = true
	if errRegister := codexoauth.Register(cfg, authManager); errRegister != nil {
		return fmt.Errorf("registering Codex client OAuth provider: %w", errRegister)
	}
	accessManager := sdkaccess.NewManager()
	accessManager.SetProviders(sdkaccess.RegisteredProviders())

	reverseProxy := httputil.NewSingleHostReverseProxy(targetURL)
	originalDirector := reverseProxy.Director
	reverseProxy.Director = func(r *http.Request) {
		originalDirector(r)
		r.Header.Set("Authorization", "Bearer "+proxyKey)
		r.Header.Del("ChatGPT-Account-ID")
	}
	reverseProxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		log.Printf("staging upstream error: %v", err)
		http.Error(w, "staging upstream unavailable", http.StatusBadGateway)
	}

	server := &http.Server{
		Addr:              *listenAddress,
		Handler:           &stagingHandler{accessManager: accessManager, authManager: authManager, proxy: reverseProxy, targetURL: targetURL, proxyKey: proxyKey, tierCapture: &tierCapture{}, accounts: make(map[string]struct{})},
		ReadHeaderTimeout: 10 * time.Second,
	}
	listener, errListen := net.Listen("tcp", *listenAddress)
	if errListen != nil {
		return fmt.Errorf("listen: %w", errListen)
	}
	fmt.Printf("ready %s\n", listener.Addr().String())

	stopContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-stopContext.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	if errServe := server.Serve(listener); errServe != nil && errServe != http.ErrServerClosed {
		return fmt.Errorf("serve: %w", errServe)
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		log.Printf("codex client OAuth staging proxy failed: %v", err)
		os.Exit(1)
	}
}
