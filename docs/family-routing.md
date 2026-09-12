# Codex family routing

`routing.strategy: family-balanced` assigns independent Codex root families across all eligible accounts, regardless of their numeric priority. Every descendant shares the root's account across models and Standard/Fast requests. The legacy strategies remain the default and rollback route.

This is a subfeature of `sdk/cliproxy/auth`. Its selector owns family membership, assignments, admission and persistence. The existing management quota monitor owns normalized observations and paced provider reads; routing receives only typed in-memory projections. The HTTP/WebSocket transports enforce the selector's pre-attempt guard. No additional service, polling loop or per-thread worker is introduced.

## Identity and allocation

The native Codex transport supplies three distinct identities:

| Field | Meaning |
| --- | --- |
| `Session-Id` / `session_id` | Root family, shared transitively by delegated descendants |
| `Thread-Id` / `thread_id` | Logical root, child or descendant |
| `X-Codex-Parent-Thread-Id` / `parent_thread_id` | Direct parent |

HTTP headers and `X-Codex-Turn-Metadata` must agree. On WebSockets, the current `response.create.client_metadata` replaces the corresponding upgrade defaults before consistency checks. Upgrade metadata can belong to prewarming or an earlier turn. Duplicate/conflicting claims, invalid identifiers, cross-family membership changes and cycles are rejected before mutation.

Context-window IDs, turn IDs, agent names and fork provenance do not define families. A context reset, physical history revert or resumed process keeps its logical family. A standalone user fork with its own `session_id == thread_id` becomes a new family even if it initially shares a context-window ID. Full-history delegated forks retain their shared root family.

A cold descendant must supply its root family or refer to a retained parent. Existing membership can recover a header-poor resume by logical thread ID. Cold root requests may omit `thread_source`. Requests with neither a session nor thread ID fail with HTTP 400; no prompt hash silently substitutes for family identity.

Allocation is atomic round robin over stable account indices after account, model, request-policy, cooldown and quota eligibility checks. A fixed pool of four eligible accounts receives five of twenty independent new families even when their priorities differ. This balances family assignments, not token use or provider throughput. A large family can still consume more work than a small one.

An existing healthy owner remains authoritative. Changing model or request policy to something its account cannot serve returns HTTP 409 instead of splitting the family. Disabled, removed, replaced, exhausted or failed owners can cause a whole-family reassignment. Mixed Codex/non-Codex provider routes and Home dispatch are explicitly unsupported for family-routed Codex execution. Ordinary non-Codex local routes continue using their legacy selector behavior.

## Weekly exclusion and observation limits

Only a valid base weekly window can establish weekly exhaustion. Additional/model-specific quota, short-window exhaustion, generic HTTP 429, capacity errors and missing values cannot become base weekly exhaustion. Unknown and stale positive observations remain unknown; they are not assumed full or used as a balancing score.

Confirmed weekly 100 percent usage remains excluded until its future reset. A later partial, short-only or positive same-window observation cannot erase it. The monitor records this evidence when adopting observations, before any family needs to select an account. Its small sibling `<monitor cache directory>.routing-weekly.v1.json` follows the existing monitor lifetime lock and flush path. Existing strict v1 entry and control shapes remain readable by older binaries. Family state also retains exclusions it has consumed, so a used assignment cannot escape the exclusion by restarting.

The routing projection independently merges weekly and five-hour scopes. A current exhausted short window excludes the account until that short reset without labeling it weekly-exhausted. Observations carry account identity, registration epoch, capture time and reset time. Replacement identities cannot inherit an old registration's binding or quota authority. Missing/unavailable projection infrastructure fails closed with HTTP 503; a healthy projection with unknown balances remains usable.

**Banked reset compatibility:** existing reset redemption remains a separate, identity-verified action. Its before/after invalidation barrier records uncertainty, not successful restoration of a weekly entitlement. This phase does not consume a trustworthy completed-redemption proof to lift a sticky weekly exclusion early. Consequently, even a later positive usage reading after a banked reset does not immediately make a previously exhausted account eligible in family mode; the recorded weekly reset must elapse. Do not delete safety state or treat an ordinary positive observation as authorization to clear it. A future early-release path needs explicit verified reset-result semantics and regression coverage.

## Admission, failover and output

New assignment/membership generations must become durable before upstream admission. Steady bound selection and transport validation perform no disk or provider reads. One change-driven writer coalesces mutations outside the selector mutex, synchronizes the file and replaces it atomically. Invalid, corrupt, oversized, conflicting or unavailable state is preserved and blocks upstream work.

Each account has a bounded active set and queue. The queue rotates among waiting families and preserves FIFO within a family, so one large fan-out cannot consume all successive slots. Cancellation releases active slots and removes queued requests. Queue overflow, acquisition expiry and persistence failure are local HTTP 503 errors; they do not cool provider credentials. The acquisition deadline ends before provider execution and is never imposed on a running stream.

Reset and failover advance assignment generations. Delayed completions cannot clear or resurrect a newer assignment. Queued old generations are rejected, while already-running streams retain their original slot until completion/cancellation. Every actual HTTP attempt, WebSocket dial and WebSocket request write rechecks the current registration, generation, model cooldown and quota in memory, including executor bootstrap reconnects.

A complete unpinned request invalidated before a provider attempt may reselect, with a separate eight-reselection bound. An account-local WebSocket continuation cannot forward `previous_response_id` to another account. Qualifying pinned pre-output failures release the pin and require a complete request on a new socket. Once the forwarder writes any downstream payload for that turn, it no longer requests full replay through the pinned failure path. Post-output failures use terminal stream handling; the router does not rerun emitted output. Client-specific reconnect behavior is a separate native-client acceptance question.

## Configuration and bounded resources

```yaml
routing:
  strategy: family-balanced
  family:
    max-concurrent-per-account: 4
    max-queued-per-account: 32
    admission-timeout: 2m
    idle-retention: 720h
    max-families: 16384
    max-members: 65536
```

Defaults are provisional operator safety settings, not measured upstream concurrency allowances. Concurrency is clamped to 64 and queued requests to 256 per account. Acquisition is clamped to 1 second–10 minutes; retention to 1–366 days. Retained family/member capacities cannot exceed 16,384/65,536. Account admission and weekly exclusion each allow at most 128 identities, matching the monitor's supported envelope. Capacity exhaustion refuses new state rather than evicting active ownership.

The default state file is `.<config filename>.family-routing-v1.json` beside the actual configuration. `family.state-file` may select an absolute path or a path relative to that configuration. One OS lock at `<state file>.lock` owns the writer lifetime; process exit releases it. The state contains namespaced hashes of family/member IDs, opaque account indices and identities, generations, timestamps and weekly exclusion evidence, with a 32 MiB maximum. It does not persist prompts, raw thread IDs, credential IDs, labels, email addresses or tokens.

Idle pruning runs in memory during selection at most hourly, skips active/queued families and removes the associated membership together. Last-seen persistence is throttled to one hour. Continuity is guaranteed only within retained state; the default idle-retention window is 30 days.

Hot reload of family settings retains the existing owner and queue. Lower active limits let current streams finish. A different state path or family/member capacity below retained usage is refused; inspect the effective runtime status rather than assuming an asynchronous config save has already changed the selector. Changing the state path requires an intentional stopped-owner restart.

## Management and Widget integration

The existing authenticated endpoints remain:

```http
GET /v0/management/routing/session-affinity
POST /v0/management/routing/session-affinity/reset
PATCH /v0/management/auth-files/fields
```

GET retains `enabled` and `session_keys` and adds `family_routing_enabled`, `effective_strategy`, `routing_guard_supported` and sanitized `family_routing` counts/limits. It exposes no membership IDs or credentials. In family mode, `session_keys` counts retained logical members.

POST reset clears assignments durably while preserving validated membership. It adds `cleared_family_assignments` and `preserved_families`; an immediate repeat clears zero assignments. Persistence failure returns HTTP 503 rather than claiming a durable reset.

Legacy automatic priority PATCHes and automatic rebinds must carry:

```http
X-CLIProxy-Routing-Guard: legacy
```

The router holds that read lease through the complete mutation. Family-mode saves, pending asynchronous reloads and selector activation exclude these operations under the same write boundary. Family mode returns HTTP 409 with exact error `family_routing_conflict` and zero mutation. Unknown guard values return HTTP 400. Unmarked manual priority edits and manual reset retain existing management authorization; the guard is not a new authentication mechanism or account compare-and-set.

**Installation order matters:** older Widget builds do not send this guard. Keep their legacy automatic routing OFF/quiesced until the guarded Widget candidate is installed, before enabling family mode. Client preflight by itself cannot close the mode-change race. A Widget talking to an older router without guard support must fail closed for automatic priority/rebind operations. Manual priority-only changes remain available.

## Verification and delivery boundary

Source regression coverage includes twenty roots/four eligible unique-priority accounts, zero exhausted provider attempts, concurrent transitive cold arrivals, HTTP and per-frame WebSocket identity, model/tier changes, resume/restart, context reset/revert versus standalone fork, state corruption and deep-cycle validation, durable admission, no steady state I/O, retention, fair bounded queues, cancellation, reset/failover generations, alias cooldown, pinned continuation refusal, monitor partial/positive/restart safety, mode-write serialization, legacy rollback and pre-output versus post-output transport errors.

The post-output WebSocket regression fails against the old forwarder on both error-channel and error-frame paths. It passes with replay suppression limited to before the first successful downstream payload write. Direct native transport evidence and exact-candidate combined CLIProxy/Widget staging are separate release acceptance checks. Their results must be recorded against the actual source and binary hashes; they must not be inferred from unit tests.

This branch is a review candidate. Canonical integration, installation, family-mode activation and protected live-port replacement require the operator's explicit green light. Preserve exact committed source, binary SHA-256, clean build metadata and the pre-change rollback artifact. Do not confuse side-branch publication or a temporary staged process with installed deployment.

## Rollback

1. Quiesce incompatible legacy automatic routing before changing modes or Widget versions.
2. Record both the saved configuration and the effective legacy selector, and retain the exact source/binary/state provenance. Older unsupported strategy strings may already normalize to a supported selector; a saved string is not proof of runtime behavior.
3. Select the supported legacy strategy that reproduces the recorded effective selector and affinity settings; verify GET reports that selector. An exact-byte configuration restoration is a separate hash-checked operation and must refuse intervening changes. Existing numeric priorities are unchanged by family allocation.
4. If a binary rollback is required, use the existing hash-locked deployment runbook only after the live action gate. Older binaries ignore the family state file and monitor sibling; keep both intact for a later forward return.
5. On re-enabling family mode, reuse the intended state path and verify persistence health. Authenticated reset may clear assignments while preserving membership. Never repair corruption by guessing ancestry or deleting live state.
