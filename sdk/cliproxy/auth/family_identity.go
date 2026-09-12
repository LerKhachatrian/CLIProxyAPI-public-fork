package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

type familyRequestIdentity struct {
	family string
	thread string
	parent string
}

func familyHash(namespace, id string) string {
	digest := sha256.Sum256([]byte(namespace + ":" + id))
	return hex.EncodeToString(digest[:])
}

func validFamilyIdentifier(value string) bool {
	if len(value) == 0 || len(value) > 160 {
		return false
	}
	for _, char := range value {
		if char < 0x21 || char > 0x7e {
			return false
		}
	}
	return true
}

// Native Codex session_id is the root/subagent family; thread_id is the logical
// agent. Context/turn/rollout IDs, agent names and user fork provenance cannot
// rename or merge that family. All redundant wire claims must agree.
func parseFamilyRequestIdentity(opts cliproxyexecutor.Options) (familyRequestIdentity, error) {
	var identity familyRequestIdentity
	headers, errHeaders := familyRequestHeaders(opts)
	if errHeaders != nil {
		return identity, errHeaders
	}
	merge := func(target *string, value string) bool {
		if value == "" {
			return true
		}
		if !validFamilyIdentifier(value) || *target != "" && *target != value {
			return false
		}
		*target = value
		return true
	}
	for name, values := range headers {
		var target *string
		switch strings.ToLower(name) {
		case "session-id", "session_id":
			target = &identity.family
		case "thread-id", "thread_id":
			target = &identity.thread
		case "x-codex-parent-thread-id":
			target = &identity.parent
		case "x-codex-turn-metadata":
			for _, value := range values {
				if len(value) > 32*1024 {
					return identity, familyRequestError("family_identity_invalid", "Codex turn metadata exceeds the family identity limit", http.StatusBadRequest)
				}
				decoder := json.NewDecoder(strings.NewReader(value))
				first, err := decoder.Token()
				if err != nil || first != json.Delim('{') {
					return identity, familyRequestError("family_identity_invalid", "invalid Codex turn metadata", http.StatusBadRequest)
				}
				seen := map[string]bool{}
				for decoder.More() {
					key, errKey := decoder.Token()
					name, ok := key.(string)
					if errKey != nil || !ok || seen[name] {
						return identity, familyRequestError("family_identity_conflict", "ambiguous Codex turn metadata", http.StatusBadRequest)
					}
					seen[name] = true
					var raw json.RawMessage
					if errDecode := decoder.Decode(&raw); errDecode != nil {
						return identity, familyRequestError("family_identity_invalid", "invalid Codex turn metadata", http.StatusBadRequest)
					}
					var field *string
					switch name {
					case "session_id":
						field = &identity.family
					case "thread_id":
						field = &identity.thread
					case "parent_thread_id":
						field = &identity.parent
					}
					if field != nil {
						var value string
						if string(raw) != "null" && json.Unmarshal(raw, &value) != nil || !merge(field, value) {
							return identity, familyRequestError("family_identity_conflict", "conflicting family or logical thread identity", http.StatusBadRequest)
						}
					}
				}
				if end, errEnd := decoder.Token(); errEnd != nil || end != json.Delim('}') {
					return identity, familyRequestError("family_identity_invalid", "invalid Codex turn metadata", http.StatusBadRequest)
				}
				if _, errEnd := decoder.Token(); errEnd != io.EOF {
					return identity, familyRequestError("family_identity_invalid", "invalid trailing Codex turn metadata", http.StatusBadRequest)
				}
			}
		}
		if target != nil {
			for _, value := range values {
				if !merge(target, value) {
					return identity, familyRequestError("family_identity_conflict", "conflicting family or logical thread identity", http.StatusBadRequest)
				}
			}
		}
	}
	if identity.thread == "" {
		identity.thread = identity.family
	}
	if identity.thread == "" {
		return identity, familyRequestError("family_identity_required", "family-balanced Codex requests require Session-Id or Thread-Id", http.StatusBadRequest)
	}
	if identity.parent == identity.thread || identity.family == identity.thread && identity.parent != "" {
		return identity, familyRequestError("family_identity_conflict", "invalid parent relationship for the logical thread", http.StatusBadRequest)
	}
	return identity, nil
}

// Native WebSocket response.create frames carry current client_metadata. The
// HTTP upgrade may be a prewarm from an earlier turn, so frame fields replace
// their corresponding handshake defaults before redundant claims are checked.
func familyRequestHeaders(opts cliproxyexecutor.Options) (http.Header, error) {
	metadata := gjson.GetBytes(opts.OriginalRequest, "client_metadata")
	if !metadata.Exists() {
		return opts.Headers, nil
	}
	if !metadata.IsObject() || len(metadata.Raw) > 32*1024 || validateFamilyJSON([]byte(metadata.Raw)) != nil {
		return nil, familyRequestError("family_identity_invalid", "invalid per-frame Codex client metadata", http.StatusBadRequest)
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(metadata.Raw), &fields) != nil {
		return nil, familyRequestError("family_identity_invalid", "invalid per-frame Codex client metadata", http.StatusBadRequest)
	}
	headers := opts.Headers.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	for key, names := range map[string][]string{
		"session_id":               {"Session-Id", "Session_id"},
		"thread_id":                {"Thread-Id", "Thread_id"},
		"x-codex-parent-thread-id": {"X-Codex-Parent-Thread-Id"},
		"x-codex-turn-metadata":    {"X-Codex-Turn-Metadata"},
	} {
		raw, exists := fields[key]
		if !exists {
			continue
		}
		var value string
		if json.Unmarshal(raw, &value) != nil {
			return nil, familyRequestError("family_identity_invalid", "per-frame Codex identity fields must be strings", http.StatusBadRequest)
		}
		for header := range headers {
			for _, name := range names {
				if strings.EqualFold(header, name) {
					delete(headers, header)
				}
			}
		}
		headers.Set(names[0], value)
	}
	return headers, nil
}

func familyCodexCandidates(provider string, auths []*Auth) bool {
	if strings.EqualFold(provider, "codex") {
		return true
	}
	for _, auth := range auths {
		if auth != nil && strings.EqualFold(auth.Provider, "codex") {
			return true
		}
	}
	return false
}
