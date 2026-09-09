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
- Automatic reset-inventory starts additionally have a 60–120-second randomized
  inter-start floor. The interval is drawn once per automatic attempt (including
  failure) and saved before dispatch. A waiting automatic reset is excluded from
  selection, so due usage and explicit manual checks retain the configured
  global 10–60-second gap. Manual reset checks do not redraw this independent
  floor. Actual automatic starts may be later because of local client cadence,
  other eligible work or cooldowns; 120 seconds is not a maximum queue latency.
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
- The automatic-reset floor uses one <=4 KiB companion named
  `<cache-directory>.automatic-resets.v1.json`, beside the existing cache. The
  same lifetime OS lock owns it. Keeping it outside the strict legacy directory
  preserves v1 control/account JSON and older-binary rollback; older binaries
  ignore, never delete, this file. Include it with the cache in backups and retain
  it across rollback/re-upgrade. Schema, duplicate/unknown fields, regular-file
  size and the recorded 60–120-second interval are validated. Invalid state
  fails closed and is preserved. It is written only when an automatic deadline
  advances, before the common budget write and provider dispatch. Failure of
  either write prevents the request; partial success conservatively retains a
  future floor. No tick, idle view, manual request or usage request rewrites an
  unchanged companion. Account removal cannot remove the shared floor.
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

## Follow-up item 1: automatic reset spacing — 2026-09-09

Session `01a077c3-26f5-7242-9d03-424a32cafc47`. This is the first separately
delivered item in the approved Widget cadence/quiet-presentation follow-up.
The existing coordinator/store owns the change. Extending the common request
gap would delay manual and usage work; changing strict cache fields would break
binary rollback. The independent, same-owner companion avoids both problems.
Daily/weekly/reset-action rules, usage classification and routing are unchanged.

Focused tests cover exact minimum/midpoint/maximum chosen intervals, repeated
clients without redraw, manual usage and reset service inside the automatic
floor, failed-attempt restart, account removal, 128-account catch-up and mixed
manual queues inside the unchanged eight-hour bound, legacy JSON/directory
compatibility, no unchanged companion writes, malformed/future/oversized state,
and failure of either pre-dispatch file write. Full Windows Go tests pass with
the three-real-Widget-client integration enabled. Immutable binary staging,
installed delivery and action-time-approved activation are still pending; tests
do not claim deployment. Both live automatic lanes remain off during this item.

## Delivery checklist

- [x] Inspect existing owners; preserve scope and freeze this contract.
- [x] Implement capture-time truth, normalized observations and durable scheduler.
- [x] Unit, concurrency, failure, lifecycle and budget tests.
- [x] Widget adapter, independent controls, local age/countdown and history truth.
- [x] Fake loopback and installed isolated-profile E2E; data preservation.
- [x] Full applicable tests/build, focused commits and fork publication.
- [x] Immutable staging candidate, hash-locked rollback, exact live preflight.
- [x] Impact warning and action-time approval, then live activation/readback.

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
Live activation passed on 2026-09-09 after the exact impact warning and Ler's
in-thread approval. Canonical PID 1271864 kept the same accepted hash and HTTP 200
after its deployment parent exited. Deployment tag `ler-live-20260909-8807b55b`
anchors the binary's exact source. The matching installed Widget 0.32 reconnected
automatically with ten account cards, both automatic lanes still off and prior
history/unrelated preferences preserved. No real provider test send, manual
monitoring queue, account action, config change or extra restart was used.
Receipts: `router-cutover01.json` and `runtime32-reconnected.json` in the existing
task evidence root. The previous hash-matched binary remains available for rollback.
This is live software acceptance, not proof of account recovery or headed human UI
acceptance. Completion dispatch follows final scoped documentation publication.

`python test/codex_monitor_staging.py --candidate <absolute-EXE>
--evidence-dir <fresh-absolute-directory>` is the feature's binary staging entry.
It restricts both listeners to 48318/48319, uses one synthetic assistant reply
with quota headers and a CONNECT-denying loopback proxy, verifies management
authentication/strict input, manual-only views and unchanged cache writes, then
waits for the real coalesced flush before an abrupt restart. It never loads real
auth or checks live accounts. The existing Fast matrix remains a separate gate.
