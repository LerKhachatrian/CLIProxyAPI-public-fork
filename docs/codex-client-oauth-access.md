# Codex Client OAuth Access

## Outcome

Allow an official Codex Desktop or CLI client to keep the built-in `openai`
provider identity while routing through a loopback CLIProxy listener. The client
authenticates with its existing ChatGPT OAuth bearer token; CLIProxy continues
to select the upstream Codex account through its normal routing pool.

```text
official Codex client (`openai`)
  -> ChatGPT OAuth bearer + ChatGPT-Account-ID
  -> loopback CLIProxy access provider
  -> normal CLIProxy Codex account selection
```

This is an opt-in access-provider feature. It creates no sidecar, watcher,
credential copier, client shim, or additional long-lived process.

## Configuration

The listener must be explicitly bound to a loopback host. An empty host,
wildcard host, or non-loopback address fails startup when this feature is
enabled.

```yaml
host: 127.0.0.1

codex:
  client-oauth-access:
    enabled: true
```

The default is disabled. Existing configured API keys remain accepted through
the existing access provider.

The repository-owned activation utility provides a zero-write plan and a
hash-bound Windows apply without printing or returning the secret-bearing
configuration:

```powershell
go build -trimpath `
  -o <staging-path>\configure_codex_client_oauth.exe `
  ./cmd/configure_codex_client_oauth

& <staging-path>\configure_codex_client_oauth.exe `
  --config <config-path> --value true --plan

& <staging-path>\configure_codex_client_oauth.exe `
  --config <config-path> --backup <new-backup-path> --value true `
  --expected-source-sha256 <plan-source-sha256> `
  --expected-candidate-sha256 <plan-candidate-sha256> --apply
```

Apply refuses drift, an existing backup, an unsafe listener, a no-op, a
non-regular configuration file, or a non-atomic platform. On Windows it uses
`ReplaceFileW` to atomically install the candidate while preserving the exact
previous file at the requested backup path. The normal watcher then applies the
change without another process restart. The utility emits only a structured
receipt containing paths, hashes, booleans, and mutation status.

## Causal evidence and architecture decision

Using Codex's supported `openai_base_url` preserves ChatGPT authentication, so
the official client sends its ChatGPT bearer token instead of a CLIProxy client
API key. Before this feature, CLIProxy interpreted that bearer as an API key and
returned `401 Unauthorized: Invalid API key`.

The access layer is the semantic owner because the incompatibility is solely
downstream-client authentication. Alternatives were rejected for lifecycle or
identity reasons:

- injecting a separate CLIProxy API key would distribute another persistent
  client secret and would not give vanilla Desktop a supported injection path;
- a permanent loopback sidecar would add a second service and failure boundary;
- a Codex core, Desktop resource, or App Server shim patch would modify the
  wrong owner and increase upgrade coupling;
- accepting every valid ChatGPT token would let another local account consume
  the configured CLIProxy pool, so the client account must already be present
  in that pool.

## Security and bounded state

Authentication fails closed unless every condition passes:

1. Configuration binds the server to an explicit loopback host.
2. The request's socket peer is loopback; forwarding headers are ignored.
3. `Authorization` is one well-formed Bearer credential and
   `ChatGPT-Account-ID` is present.
4. The account ID matches an enabled Codex OAuth account already loaded in the
   runtime pool.
5. OpenAI's Codex model endpoint accepts that exact bearer/account pair.

Successful validations are cached for one minute by SHA-256 of the bearer and
account pair. The cache retains at most 256 hashes, never raw tokens or account
IDs. Identical concurrent validations are deduplicated and at most eight unique
OpenAI validations run concurrently. Pool membership is checked again on every
request, including cache hits, so disabling an account takes effect
immediately. The lookup runs under the auth manager's read lock and does not
clone token-bearing credential metadata.

The access result exposes only a truncated hash-derived principal and a static
source label. Tokens, account IDs, prompts, request bodies, and provider payloads
are not logged or persisted by this feature.

## Lifecycle and rollback

- Startup registers the provider only after the runtime auth catalog exists.
- Configuration reload replaces or removes the provider through the existing
  access-provider reconciliation path.
- Disabling `codex.client-oauth-access.enabled` removes it without changing
  auth files, account routing, client state, or the API-key provider.
- Changing the listener to a non-loopback host while enabled fails closed.

Rollback is configuration-only: set `enabled: false` and restore the prior
binary through the normal hash-locked deployment runbook. No state conversion
or credential restoration is required.

## Verification contract

Minimum code gates:

```powershell
go test -count=1 ./cmd/configure_codex_client_oauth ./internal/access/codex_oauth ./internal/access ./internal/config ./sdk/cliproxy ./internal/api ./test/codex_client_oauth_staging_proxy
go test -race -count=1 ./internal/access/codex_oauth
go vet ./cmd/configure_codex_client_oauth ./internal/access/codex_oauth ./internal/access ./internal/config ./sdk/cliproxy ./internal/api ./test/codex_client_oauth_staging_proxy
go build -trimpath -o <staging-path>\configure_codex_client_oauth.exe ./cmd/configure_codex_client_oauth
go test -count=1 ./...
go build -trimpath -o <staging-path>\cliproxyapi.exe ./cmd/server
```

The temporary `test/codex_client_oauth_staging_proxy` validates a real official
Codex login on loopback and forwards accepted requests to an already-running
CLIProxy using its existing operator credential. It dynamically admits the
validated client account only inside the disposable process; production never
does this.

Its `GET /__staging_tiers` and `DELETE /__staging_tiers` endpoints expose a
bounded in-memory ring containing only sequence numbers and sanitized service
tier classifications. The verifier observes both HTTP request bodies and
WebSocket `response.create` frames. Inspection is limited to 1 MiB per small
canary request or frame; bytes are forwarded unchanged and are never written to
disk or returned by the endpoint. This proves the official client sends no
priority tier for Standard and sends Fast intent, without retaining prompts.

Combine that client proof with `test/fast_mode_staging.ps1`, which must prove the
candidate router forwards Fast as `service_tier: priority` and omits the field
for Standard. Final acceptance requires the exact immutable candidate on
staging, then a separately approved live activation and real Desktop/CLI
Standard, Fast, file-tool, restart, and account-rotation checks.
