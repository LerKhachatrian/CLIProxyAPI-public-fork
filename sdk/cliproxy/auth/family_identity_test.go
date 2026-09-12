package auth

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestFamilyIdentityNativeHeadersAndCurrentWebSocketFrame(t *testing.T) {
	opts := familyOptions("root", "child", "root")
	opts.Headers.Set("X-Codex-Turn-Metadata", `{"session_id":"root","thread_id":"child","parent_thread_id":"root","context_window_id":"old-context","thread_source":"subagent"}`)
	identity, err := parseFamilyRequestIdentity(opts)
	if err != nil || identity != (familyRequestIdentity{family: "root", thread: "child", parent: "root"}) {
		t.Fatalf("native HTTP identity: %+v %v", identity, err)
	}
	frame := map[string]any{"type": "response.create", "client_metadata": map[string]string{
		"session_id": "root", "thread_id": "grandchild", "x-codex-parent-thread-id": "child",
		"x-codex-turn-metadata": `{"session_id":"root","thread_id":"grandchild","parent_thread_id":"child","context_window_id":"new-context"}`,
	}}
	opts.OriginalRequest, _ = json.Marshal(frame)
	identity, err = parseFamilyRequestIdentity(opts)
	if err != nil || identity != (familyRequestIdentity{family: "root", thread: "grandchild", parent: "child"}) {
		t.Fatalf("current frame did not supersede prewarm handshake: %+v %v", identity, err)
	}
	if opts.Headers.Get("Thread-Id") != "child" {
		t.Fatal("frame parsing mutated shared handshake headers")
	}
}

func TestFamilyIdentityRejectsAmbiguousClaims(t *testing.T) {
	for _, tc := range []struct{ name, metadata, body, parent string }{
		{name: "different root", metadata: `{"session_id":"other"}`},
		{name: "duplicate claim", metadata: `{"thread_id":"child","thread_id":"child"}`},
		{name: "nonstring thread", metadata: `{"thread_id":5}`},
		{name: "trailing JSON", metadata: `{} {}`},
		{name: "oversized metadata", metadata: strings.Repeat(" ", 32*1024+1)},
		{name: "self parent", parent: "child"},
		{name: "duplicate frame claim", body: `{"client_metadata":{"thread_id":"child","thread_id":"other"}}`},
		{name: "invalid frame claim", body: `{"client_metadata":{"session_id":7}}`},
		{name: "frame disagrees internally", body: `{"client_metadata":{"thread_id":"other","x-codex-turn-metadata":"{\"thread_id\":\"child\"}"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := familyOptions("root", "child", "root")
			if tc.metadata != "" {
				opts.Headers.Set("X-Codex-Turn-Metadata", tc.metadata)
			}
			if tc.parent != "" {
				opts.Headers.Set("X-Codex-Parent-Thread-Id", tc.parent)
			}
			opts.OriginalRequest = []byte(tc.body)
			if _, err := parseFamilyRequestIdentity(opts); statusCodeFromError(err) != http.StatusBadRequest {
				t.Fatalf("accepted ambiguous identity: %v", err)
			}
		})
	}
	missing := familyOptions("", "", "")
	if _, err := parseFamilyRequestIdentity(missing); statusCodeFromError(err) != http.StatusBadRequest {
		t.Fatalf("missing identity: %v", err)
	}
}

func TestFamilyMembershipRejectsConflictsWithoutMutation(t *testing.T) {
	f := newFamilyFixture(t, 2, FamilyRoutingConfig{})
	f.pick(t, "root", "grandchild", "child", "model")
	f.pick(t, "root", "child", "root", "model")
	before := f.router.status()
	for _, identity := range []familyRequestIdentity{
		{family: "other", thread: "child", parent: "other"},
		{family: "root", thread: "child", parent: "grandchild"},
		{family: "root", thread: "grandchild", parent: "root"},
		{thread: "unknown-child", parent: "unknown-parent"},
	} {
		opts := familyOptions(identity.family, identity.thread, identity.parent)
		if _, err := f.router.pick(t.Context(), "model", opts, f.auths); statusCodeFromError(err) != http.StatusConflict {
			t.Fatalf("accepted conflicting membership %+v: %v", identity, err)
		}
		if after := f.router.status(); after.Families != before.Families || after.Members != before.Members {
			t.Fatalf("rejected claims mutated retained membership: %+v", after)
		}
	}
}

func TestFamilyIdentityContextResetRevertAndStandaloneFork(t *testing.T) {
	f := newFamilyFixture(t, 2, FamilyRoutingConfig{})
	var original string
	for _, window := range []string{"first", "after-context-reset", "after-revert"} {
		opts := familyOptions("root", "root", "")
		opts.Headers.Set("X-Codex-Turn-Metadata", `{"thread_id":"root","context_window_id":"`+window+`"}`)
		a, err := f.router.pick(t.Context(), "model", opts, f.auths)
		if err != nil {
			t.Fatal(err)
		}
		if original == "" {
			original = a.ID
		}
		if a.ID != original {
			t.Fatal("logical root changed with context/history replacement")
		}
	}
	fork := familyOptions("user-fork", "user-fork", "")
	fork.Headers.Set("X-Codex-Turn-Metadata", `{"thread_id":"user-fork","context_window_id":"first"}`)
	a, err := f.router.pick(t.Context(), "model", fork, f.auths)
	if err != nil || a.ID == original || f.router.status().Families != 2 {
		t.Fatalf("standalone user fork reused context family: %v", err)
	}
}
