package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestObserveResponseTierPreservesBodyAndCapturesOnlyTier(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantTier string
	}{
		{
			name:     "standard omitted",
			body:     `{"model":"gpt-5.6-sol","input":"private prompt"}`,
			wantTier: "absent",
		},
		{
			name:     "fast",
			body:     `{"input":"private prompt","service_tier":"fast"}`,
			wantTier: "fast",
		},
		{
			name:     "priority",
			body:     `{"service_tier":"priority","input":"private prompt"}`,
			wantTier: "priority",
		},
		{
			name:     "unknown value redacted",
			body:     `{"service_tier":"private-tier","input":"private prompt"}`,
			wantTier: "other",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/responses", strings.NewReader(test.body))
			gotTier, observed := observeResponseTier(req)
			if !observed || gotTier != test.wantTier {
				t.Fatalf("observeResponseTier() = %q, %v; want %q, true", gotTier, observed, test.wantTier)
			}
			gotBody, errRead := io.ReadAll(req.Body)
			if errRead != nil {
				t.Fatalf("reading restored body: %v", errRead)
			}
			if string(gotBody) != test.body {
				t.Fatal("observeResponseTier changed the forwarded request body")
			}
		})
	}
}

func TestObserveResponseTierBoundsInspectionAndPreservesOversizedBody(t *testing.T) {
	body := `{"input":"` + strings.Repeat("x", maxTierCaptureBody) + `"}`
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/responses", strings.NewReader(body))
	gotTier, observed := observeResponseTier(req)
	if !observed || gotTier != "unavailable-oversized" {
		t.Fatalf("observeResponseTier() = %q, %v; want unavailable-oversized, true", gotTier, observed)
	}
	gotBody, errRead := io.ReadAll(req.Body)
	if errRead != nil {
		t.Fatalf("reading restored oversized body: %v", errRead)
	}
	if string(gotBody) != body {
		t.Fatal("observeResponseTier changed the oversized forwarded request body")
	}
}

func TestObserveWebSocketTierCapturesOnlyResponseCreate(t *testing.T) {
	tests := []struct {
		name        string
		payload     string
		truncated   bool
		wantTier    string
		wantObserve bool
	}{
		{name: "standard", payload: `{"type":"response.create","input":"private prompt"}`, wantTier: "absent", wantObserve: true},
		{name: "fast", payload: `{"type":"response.create","service_tier":"fast","input":"private prompt"}`, wantTier: "fast", wantObserve: true},
		{name: "append ignored", payload: `{"type":"response.append","service_tier":"fast"}`},
		{name: "invalid ignored", payload: `not-json`},
		{name: "oversized create classified", payload: `{"type":"response.create","input":"`, truncated: true, wantTier: "unavailable-oversized", wantObserve: true},
		{name: "oversized append ignored", payload: `{"type":"response.append","input":"`, truncated: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gotTier, gotObserve := observeWebSocketTier([]byte(test.payload), test.truncated)
			if gotTier != test.wantTier || gotObserve != test.wantObserve {
				t.Fatalf("observeWebSocketTier() = %q, %v; want %q, %v", gotTier, gotObserve, test.wantTier, test.wantObserve)
			}
		})
	}
}

func TestBoundedCaptureBufferNeverRetainsMoreThanLimit(t *testing.T) {
	var capture boundedCaptureBuffer
	payload := strings.Repeat("x", maxTierCaptureBody+1024)
	written, errWrite := capture.Write([]byte(payload))
	if errWrite != nil || written != len(payload) {
		t.Fatalf("Write() = %d, %v; want %d, nil", written, errWrite, len(payload))
	}
	if !capture.truncated || capture.buffer.Len() != maxTierCaptureBody {
		t.Fatalf("capture = truncated:%v bytes:%d", capture.truncated, capture.buffer.Len())
	}
}

func TestTierCaptureIsBoundedAndReturnsCopies(t *testing.T) {
	capture := &tierCapture{}
	for range maxTierCaptureEvents + 5 {
		capture.record("absent")
	}
	events := capture.snapshot()
	if len(events) != maxTierCaptureEvents {
		t.Fatalf("event count = %d, want %d", len(events), maxTierCaptureEvents)
	}
	events[0].Tier = "mutated"
	if got := capture.snapshot()[0].Tier; got != "absent" {
		t.Fatalf("snapshot mutated capture state: %q", got)
	}
	capture.reset()
	if got := len(capture.snapshot()); got != 0 {
		t.Fatalf("event count after reset = %d, want 0", got)
	}
}
