package openai

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestFamilyWebsocketReplaySuppressionStopsAfterDownstreamPayload(t *testing.T) {
	for _, channelError := range []bool{false, true} {
		for _, emitted := range []bool{false, true} {
			t.Run(fmt.Sprintf("channel=%t/emitted=%t", channelError, emitted), func(t *testing.T) {
				type outcome struct {
					callbacks int
					terminal  bool
					err       error
				}
				finished := make(chan outcome, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					conn, err := responsesWebsocketUpgrader.Upgrade(w, r, nil)
					if err != nil {
						finished <- outcome{err: err}
						return
					}
					defer func() { _ = conn.Close() }()
					data := make(chan []byte)
					errs := make(chan *interfaces.ErrorMessage)
					go func() {
						if emitted {
							select {
							case data <- []byte(`{"type":"response.output_text.delta","delta":"committed"}`):
							case <-r.Context().Done():
								return
							}
						}
						if channelError {
							select {
							case errs <- &interfaces.ErrorMessage{StatusCode: http.StatusTooManyRequests, Error: errors.New("synthetic capacity")}:
							case <-r.Context().Done():
							}
						} else {
							select {
							case data <- []byte(`{"type":"error","status":429,"error":{"code":"rate_limit_exceeded","type":"rate_limit_error","message":"synthetic capacity"}}`):
							case <-r.Context().Done():
							}
						}
					}()
					ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
					ctx.Request = r
					h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))
					callbacks := 0
					_, _, _, errMsg, errForward := h.forwardResponsesWebsocket(ctx, newResponsesWebsocketWriter(conn), func(...interface{}) {}, data, errs, newInMemoryWebsocketTimelineLog(), "synthetic-family",
						responsesWebsocketForwardOptions{suppressError: func(*interfaces.ErrorMessage) bool { callbacks++; return true }})
					finished <- outcome{callbacks: callbacks, terminal: errMsg != nil, err: errForward}
				}))
				defer server.Close()
				client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = client.Close() }()
				_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
				payloads := 0
				for {
					if _, _, err := client.ReadMessage(); err != nil {
						break
					}
					payloads++
				}
				select {
				case result := <-finished:
					if !result.terminal {
						t.Fatal("upstream error was lost")
					}
					if emitted {
						if result.callbacks != 0 || !errors.Is(result.err, websocket.ErrCloseSent) || payloads != 1 {
							t.Fatalf("committed output requested replay: callbacks=%d payloads=%d err=%v", result.callbacks, payloads, result.err)
						}
					} else if result.callbacks != 1 || result.err != nil || payloads != 0 {
						t.Fatalf("pre-output replay was blocked: %+v payloads=%d", result, payloads)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("forwarder did not finish")
				}
			})
		}
	}
}
