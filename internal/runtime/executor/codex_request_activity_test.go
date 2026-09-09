package executor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestCodexWebsocketRequestActivityThroughManager(t *testing.T) {
	for _, mode := range []string{"execute", "stream", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			started, firstPayload := make(chan struct{}), make(chan struct{})
			bootstrap, completion := make(chan struct{}), make(chan struct{})
			releaseBootstrap := sync.OnceFunc(func() { close(bootstrap) })
			releaseCompletion := sync.OnceFunc(func() { close(completion) })
			upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
				close(started)
				select {
				case <-bootstrap:
				case <-ctx.Done():
					return
				}
				if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"synthetic-activity","output":[]}}`)); err != nil {
					return
				}
				close(firstPayload)
				select {
				case <-completion:
				case <-ctx.Done():
					return
				}
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"synthetic-activity","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ACTIVITY_WS_COMPLETE"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`))
			}))
			executor := NewCodexWebsocketsExecutor(&config.Config{})
			executor.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
			m := cliproxyauth.NewManager(nil, nil, nil)
			m.SetRetryConfig(0, 0, 1)
			m.RegisterExecutor(executor)
			a, err := m.Register(ctx, &cliproxyauth.Auth{ID: "synthetic-ws-activity-" + mode, Provider: "codex", Status: cliproxyauth.StatusActive,
				Attributes: map[string]string{"api_key": "synthetic-only", "base_url": server.URL, "websockets": "true"}})
			if err != nil {
				cancel()
				server.Close()
				t.Fatal(err)
			}
			model := "gpt-5.6-sol"
			registry.GetGlobalRegistry().RegisterClient(a.ID, "codex", []*registry.ModelInfo{{ID: model}})
			var worker sync.WaitGroup
			done := make(chan error, 1)
			t.Cleanup(func() {
				cancel()
				releaseBootstrap()
				releaseCompletion()
				worker.Wait()
				server.Close()
				registry.GetGlobalRegistry().UnregisterClient(a.ID)
				executor.store.mu.Lock()
				ids := make([]string, 0, len(executor.store.sessions))
				for id := range executor.store.sessions {
					ids = append(ids, id)
				}
				executor.store.mu.Unlock()
				for _, id := range ids {
					executor.CloseExecutionSession(id)
				}
			})
			worker.Go(func() {
				req := cliproxyexecutor.Request{Model: model, Payload: []byte(`{"model":"gpt-5.6-sol","input":"synthetic activity probe"}`)}
				opts := cliproxyexecutor.Options{Stream: mode != "execute", SourceFormat: sdktranslator.FromString("codex")}
				var resultErr error
				ack := false
				if mode == "execute" {
					response, err := m.Execute(ctx, []string{"codex"}, req, opts)
					resultErr, ack = err, strings.Contains(string(response.Payload), "ACTIVITY_WS_COMPLETE")
				} else {
					stream, err := m.ExecuteStream(ctx, []string{"codex"}, req, opts)
					resultErr = err
					if stream != nil {
						for chunk := range stream.Chunks {
							ack = ack || strings.Contains(string(chunk.Payload), "ACTIVITY_WS_COMPLETE")
							if chunk.Err != nil {
								resultErr = chunk.Err
							}
						}
					}
				}
				if mode != "cancel" && !ack && resultErr == nil {
					resultErr = errors.New("missing synthetic WebSocket assistant reply")
				}
				done <- resultErr
			})
			for _, phase := range []<-chan struct{}{started, firstPayload} {
				select {
				case <-phase:
				case err := <-done:
					t.Fatal("WebSocket ended before the held phase", err)
				case <-ctx.Done():
					t.Fatal("WebSocket phase did not start")
				}
				if got := a.RequestActivitySnapshot(); got.InFlight != 1 || !got.LastCompleted.IsZero() {
					t.Fatal("actual WebSocket bootstrap/first payload lost active scope")
				}
				releaseBootstrap()
			}
			if mode == "cancel" {
				cancel()
			} else {
				releaseCompletion()
			}
			if err := <-done; err != nil && mode != "cancel" {
				t.Fatal(err)
			}
			if got := a.RequestActivitySnapshot(); got.InFlight != 0 || got.LastCompleted.IsZero() {
				t.Fatal("WebSocket completion/cancellation leaked or lost activity")
			}
		})
	}
}
