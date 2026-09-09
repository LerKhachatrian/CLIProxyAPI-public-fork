# Passive-first Codex quota monitoring

## Decision and scope

This is a management subfeature, owned by
`internal/api/handlers/management/codexmonitor`. The existing auth manager owns
ordinary-request observations and routing; the monitor owns only read scheduling
and an allowlisted display cache. It never changes credential, priority,
affinity, cooldown, redemption, or generation state.

Widget-local sweep timers cannot coordinate multiple consumers and repeat reset
inventory reads unnecessarily. An additional broker would duplicate the router's
credential and lifecycle boundary. The selected design extends the existing
router with one shared, persistent, request-driven coordinator. There is no new
service, background ticker, provider traffic while all consumers are closed, or
token ledger. Clients sharing this router share its budgets. Independent routers
holding the same accounts are **not** coordinated: all monitoring clients for a
pool must use its single owning router, including clients on another host.

The supported envelope is 128 Codex accounts, 256 reset grants per account and
4096 grants in the pool. These are explicit limits, not a claim of 10,000-account
support. Ordinary response coverage is provider-dependent; a quota timestamp
alone does not imply complete windows. Retry-After-only and additional-model-only
signals cannot become a main entitlement. Missing five-hour allowance is unknown,
not zero. No evidence establishes that polling caused any provider challenge, or
that these intervals prevent one.

## Contract and lifecycle

- `GET /v0/management/codex/quota-monitor`: local snapshots only, no provider
  reads. Ordinary HTTP/WS observations keep their capture timestamps rather than
  the later completion or Widget-fetch time. Only valid reported fields survive.
- `POST /v0/management/codex/quota-monitor`: one bounded scheduling step. A
  client supplies usage fallback and reset-inventory preferences; it cannot
  choose a URL, token, proxy, credential mutation, or provider request body.
- Independent manual usage/reset refresh requests queue at most one intent per
  eligible account/lane. Repeated requests coalesce. A step claims at most one
  intent or due automatic read. There is no whole-pool startup/catch-up sweep.
  A whole-lane batch coalesces until its final pending/in-flight account finishes;
  its bounded eight-hour lifetime covers two maximum-gap 128-account lanes.
  Expiry becomes an explicit refresh-expired state, not a silently lost request.
- Active accounts and the two highest-priority eligible candidates use a
  configurable 30-minute fallback target with 20–40-minute jitter at the default.
  Other accounts use a roughly daily interval. Recent useful passive main-window
  observations postpone fallback; missing coverage still remains explicit.
- Reset inventory is independent: daily, weekly, or manual-only. Every attempt,
  including failure, advances automatic eligibility by at least 24 hours (or the
  requested longer interval). An explicit manual request may bypass that cadence,
  but never cooldowns or the one-minute duplicate-attempt floor. Redemption's
  separate fresh preflight is unchanged.
- One monitoring provider read in flight, at least ten seconds between starts
  (clients may request a larger gap), and six starts per rolling minute. Oldest
  due work wins; jitter, persisted deadlines and a global budget prevent restart,
  wake, reconnect and multi-Widget bursts. Manual work is paced, not instantaneous.
- 401/403/429/transient errors retain separate lane errors and last-known data.
  Retry-After and router exclusions are floors. No challenge bypass, generation
  test, automatic reconnect, quota-state clear or reset redemption is performed.
- Before dispatch, persist the attempt and next deadline. A crash after dispatch
  cannot replay an automatic reset check. Persistence failure blocks new reads;
  malformed/newer cache formats are preserved and fail closed, never erased.
- The cache is separate from auth files, alongside the router config, and stores
  hashed identities, normalized quota/grant observations and schedule state only.
  It contains no tokens, email, account labels, prompts, URLs, or raw responses.
  Per-account atomic files avoid whole-pool rewrite amplification. One OS lock
  holds the cache's exclusive lifetime; a competing router fails closed. Lock
  ownership ends on orderly shutdown or process death. No stale-lock deletion.
  Passive display updates are coalesced to at most one flush per minute, driven
  by local views (or orderly router close). Abrupt failure can lose an unflushed
  display observation; it must never rejuvenate the older retained capture.
  Provider attempts, action barriers and their safety deadlines are persisted
  synchronously before dispatch and do not share this display-only loss window.
- Removed/replaced/disabled identities lose display authority. A reported reset
  or grant expiry expires the corresponding balance without a new provider call.
  Usage and reset ages remain independent; cached samples never become fresh
  projection, calibration, or reset-action evidence.
- Identity-verified fixed-route explicit reads can supply newer observations
  and advance automatic deadlines. A consume operation invalidates both display
  lanes before dispatch and after every outcome; a persisted timestamp barrier
  refuses older passive/in-flight data across restart. The monitor never sends
  or retries that action and does not infer redemption from its response.
- Failed lanes retain their failure capture time. A newer valid success can
  supersede an obsolete OAuth demand, but the other balance still needs its own
  observation and every existing Retry-After/cadence floor remains intact.

## Frozen acceptance contract (before implementation)

Baseline: Widget 0.31 performed up to two provider GETs per eligible account per
five-minute-plus-sweep cycle. Its memory cache and schedule did not survive
restart. Baseline passive live coverage and idle resource measurements are gaps.

Required proof uses fake clocks and synthetic transports, never live quota or
credits: 128-account fair service; concurrent clients; restart/abrupt-dispatch
recovery; malformed state; identity changes; 401/403/429/5xx and long Retry-After;
partial/absent/valid-zero quota signals; separate daily/weekly/manual resets;
local countdown expiry; one-flight/six-per-minute limits; zero provider reads in
manual-only without an explicit queue; failed disk writes before dispatch.

Performance targets are operational bounds: no new background process/timer;
no network or cache I/O on generation or the UI thread; at most 8 seconds per
monitoring provider request and 2 MiB response bodies; local 128-account snapshot
p95 under 250 ms on the target Windows machine; cache at most 128 account files,
one small control file and one OS-lock file, with a 16 MiB aggregate cap. Measure
peak/retained memory and writes during a bounded 128-account workload. The UI's
existing heartbeat <100 ms and asynchronous close <11 seconds remain required.
These are bounded-workload claims, not universal no-leak/no-freeze promises.

## Delivery checklist

- [x] Inspect existing owners; preserve scope and freeze this contract.
- [x] Implement capture-time truth, normalized observations and durable scheduler.
- [x] Unit, concurrency, failure, lifecycle and budget tests.
- [x] Widget adapter, independent controls, local age/countdown and history truth.
- [x] Fake loopback and installed isolated-profile E2E; data preservation.
- [x] Full applicable tests/build, focused commits and fork publication.
- [x] Immutable staging candidate, hash-locked rollback, exact live preflight.
- [ ] Impact warning and action-time approval, then live activation/readback.

The monitor is additive. A compatible older router causes new clients to show an
upgrade requirement and make no legacy provider sweeps. Binary rollback leaves
the monitor cache ignored, not deleted, and does not downgrade user databases.
Deployment details remain in `ler-fork-maintenance.md`.

### Current synthetic evidence — 2026-09-08

Full Windows Go tests pass, including the actual Widget transport fixture with
three clients. Two reset reads, zero usage reads and 30 repeated clicks retain
the shared ten-second minimum and capture times across cache restart; real
offscreen Widget rendering distinguishes missing main 5h, zero and age expiry.
At 128 accounts/4096 grants, 1000 cached views measured p95 1.074 ms/max 1.507 ms,
722174 serialized bytes, ~5.5 MB sampled peak heap and zero idle writes. The
final post-recovery-change scheduler race test passed under Linux Go 1.26.2 in
3.467 seconds. No native Windows race or
physical-disk-soak claim is made. No live account or irreversible action was used.
Final build/source provenance, immutable staging, action-time approval and live
activation are separate release gates, not implied by these measurements. The
final build/staging/preflight gates now pass at source `8807b55b5c8f4cff7a3c430ff5988d288bf473ee`,
SHA-256 `8e10e3005ec82369942711e75b663a021450fee9c262d734b79ecc8a6253de13`.
Only action-time-approved live activation and subsequent independent native
readback remain pending. The installed Widget keeps both automatic lanes off;
the old router cannot supply its new quota-monitor view until that cutover.

`python test/codex_monitor_staging.py --candidate <absolute-EXE>
--evidence-dir <fresh-absolute-directory>` is the feature's binary staging entry.
It restricts both listeners to 48318/48319, uses one synthetic assistant reply
with quota headers and a CONNECT-denying loopback proxy, verifies management
authentication/strict input, manual-only views and unchanged cache writes, then
waits for the real coalesced flush before an abrupt restart. It never loads real
auth or checks live accounts. The existing Fast matrix remains a separate gate.
