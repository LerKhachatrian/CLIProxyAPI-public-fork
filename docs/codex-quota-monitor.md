# Passive-first Codex quota monitoring

## Initial reset inventory — deployed and accepted

Session `01a077c3-26f5-7242-9d03-424a32cafc47`, 2026-09-09. A never-checked reset bank previously received a random first deadline up to 24 hours away, so a correct partial total could stay incomplete until the next day. This is a scheduling problem, not lost credits or incorrect summation. With reset Auto enabled, new missing inventory is immediately eligible, and existing strict-v1 entries with no bank and no prior attempt have their future bootstrap deadlines brought forward once. Snapshot GETs remain local-only; the next ordinary scheduling step may claim one read.

The existing coordinator owns this small subfeature. A manual catch-up would not repair future new/upgraded accounts; another worker or client-side sweep would duplicate shared pacing and persistence. First checks use the same one-flight, common 10–60-second/six-per-minute budget and independent randomized 60–120-second automatic-reset floor. A ten-account pool therefore needs a paced sequence, not one burst; client cadence, other work and cooldowns can extend completion. Manual-only still makes zero automatic reads. Existing observed banks, failed or interrupted attempts, disabled/blocked identities, Retry-After, action authority and daily/weekly recurring cadence are unchanged. No schema, timer, new process or cache-file migration is added.

Regression coverage includes fresh and legacy pools of 10 and 128 accounts under Daily/Weekly, durable pre-dispatch claims, manual-to-Auto transition, zero-write local views, valid zero, exclusions, failure/restart and strict-cache write failure. The real three-Widget-client fixture upgrades two legacy entries, displays a synthetic partial 4 then complete 19, preserves actual capture times across two restarts, and records a 75,361-ms actual gap without manual intents. That synthetic total is not a live-provider balance claim. The existing binary harness also offers `--initial-inventory`: its loopback proxy deliberately denies one initial attempt, then checks daily failure cadence and no replay after restart. Full/race/build, exact-candidate staging and independent live acceptance now pass as recorded below. Existing 128-account/4096-grant p95 <250 ms, 16 MiB cache, <100-ms UI and <11-second close budgets remain unchanged; Widget application/package inputs are unchanged.

### Current live acceptance — 2026-09-09 15:24 -04:00

Session `01a077c3-26f5-7242-9d03-424a32cafc47`. The approved router-only fix is deployed from clean source `184b4b3c903fce5d6188c7d17a01a92c083e006b`, immutable source tag `ler-live-20260909-184b4b3c`, Go 1.26.2 Windows/amd64 with CGO disabled, trimpath and `vcs.modified=false`. Canonical SHA-256 is `a4c620a5be565ab265e79cf40faf5f8e0f75451f0aebab46456b5ba391029f2a`. Hash-locked preflight and the repeated impact warning preceded the approved cutover. The exact canonical process independently serves HTTP 200 after its deployment parent exits. The dedicated previous `cb2e8442` binary remains hash-verified for rollback; only its redundant displaced copy was removed. No configuration, credentials, account actions, affinity, Desktop, SSH or App Server settings changed.

The full Windows Go rerun passes with the accepted Widget source, including both actual three-client integrations. The first run's existing file-store replacement failure remains recorded; twenty focused repeats and the full rerun pass after adding test-owner cleanup, without a production retry or weaker assertion. Its OS cause is not established. The affected monitor/management packages pass Linux Go 1.26.2 race tests; native Windows race is not claimed. At 128 accounts/4,096 grants/1,000 local views, p95 is 1.0009 ms, maximum 15.8432 ms, serialized state 732,024 bytes, sampled peak heap 5,104,512 bytes and idle writes zero. Exact-binary initial-upgrade/failed-attempt/no-replay, passive HTTP/stream/restart, Fast, affinity and external-respawner/forced-rollback staging pass on 48318/48319 with all fixture owners stopped and no real-account QA requests.

At 19:23:44 UTC, the unchanged independently running Widget 0.38 displays `~≥18`: nine of ten banks are observed, all four previously never-attempted entries have now been attempted, and none remains waiting for an initial deadline. Their durable admission timestamps span 19:13:55–19:19:55 UTC; these are not packet-level dispatch measurements. One reset read returned HTTP 401 (`oauth_rejected`), leaving that account's bank unknown. This is successful paced bootstrap with an explicit account-authentication limitation, not proof of a complete total of 19 or lost credits. No forced retry, manual refresh, OAuth operation or live-cache edit was used; failure cadence and all cooldowns remain intact. Review/recover that account separately before expecting complete coverage.

Widget was neither rebuilt nor restarted: its installed 0.38 SHA-256 remains `970025b1e36edefe766fc9a8df175add16579886cfdb0271d8d4d63572551461`, with the same responsive, minimized, nonforeground UI process and ten cards. Auto usage/reset/common gap remains 1,800,000/86,400,000/10,000 ms and Strong highlighting remains false. All prior reset events, operation rows and projection samples are hash-identical; only normal derived subscription tracking changed. The count-only verifier made zero provider calls, manual intents or native actions. Evidence is in the existing `05-initial-reset` acceptance bundle (`full02`, `race01`, `build01`, `monitor01`, `fast01`, `affinity01`, `cutover01`, `cutover-live01`, `independent-router01`, `installed_before01` and `installed_after03`). Final delivery changes Markdown only; human headed/DPI/actual Restart acceptance stays deferred in Widget's single existing checklist.

## Prior activity-aware acceptance — 2026-09-09 14:37 -04:00

Session `01a077c3-26f5-7242-9d03-424a32cafc47`. The activity-aware release is now live, superseding its historical pending gates below. Ler's explicit greenlight covered the disclosed brief router interruption and Widget installation. Fresh preflight verified the immutable staged candidate, canonical listener, health and rollback, and the impact warning was repeated before execution. Clean source `fdfa6ae8b867ecd31d81a9cc877cd478ab05d687`, source tag `ler-live-20260909-fdfa6ae8`, maps to canonical SHA-256 `cb2e84421a4e26b98667beff0174be459b2c546108e3f614efe3d4b6cd2c1a91`. PID 1946164 independently serves HTTP 200 after the deployment owner exits. The hash-matched `56ff39cd` rollback remains retained; only the redundant displaced copy was removed.

Installed Widget 0.38 independently reconnects ten accounts after its short-lived launcher exits. Both Auto lanes, all saved settings, full-email hover coverage and prior history pass readback without verifier provider/manual/action requests. Normal scheduled operation is authorized, not proof of every provider balance. Full/race, real-client and binary activity/Fast/affinity/forced-rollback staging evidence remains unchanged. Human headed/DPI/actual Restart checks remain deferred in Widget's existing acceptance checklist. Initial unknown reset inventory can still wait for its old 0–24-hour startup deadline; the now-approved startup-inventory fix is a separate subsequent item, not part of this binary.

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
- Actually active accounts use 5–10-minute fallback. The two highest-priority
  eligible likely-next candidates use a configurable 30-minute target with
  20–40-minute jitter at the default; reserves use 20–28 hours. Every in-flight
  account and every account used within the last hour qualifies as active,
  including round-robin use. Recent useful passive main-window
  observations postpone fallback; missing coverage still remains explicit.
- Reset inventory is independent: daily, weekly, or manual-only. Every attempt,
  including failure, advances automatic eligibility by at least 24 hours (or the
  requested longer interval). An explicit manual request may bypass that cadence,
  but never cooldowns or the one-minute duplicate-attempt floor. Redemption's
  separate fresh preflight is unchanged.
- With reset Auto enabled, never-attempted missing inventory is eligible on the
  next scheduling step, including legacy future bootstrap deadlines. It still
  passes every shared budget and exclusion; this is not a whole-pool sweep.
  Existing banks and any prior attempt retain their durable recurring cadence.
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

### Follow-up item 4: activity-aware fallback design — 2026-09-09

Session `01a077c3-26f5-7242-9d03-424a32cafc47`. The preceding Widget tooltip item is delivered separately. This item is a subfeature of the existing auth request lifecycle and monitor scheduler; its live activation remains gated by fresh staging, hash-locked preflight, an interruption warning and action-time approval.

The current adapter combines six completed-request buckets with the two highest eligible priorities. Buckets miss requests that are still running, and the combined flag cannot distinguish real work from likely-next candidates. Home's execution registry is not a suitable replacement because it does not cover the local selector path. Extend the existing transport-attempt context and auth lifecycle instead: one memory-only activity cell per registered Codex identity, shared by its clones. An actual upstream transport attempt starts a once-only scope; completion, error, cancellation or stream retirement ends it. Selection, preparation, token counting, management reads and idle WebSocket connections do not create activity. No request body, token, label or address enters this cell. Normal metadata/priority refresh preserves it, while account replacement, removal/re-registration, disabling and process restart retire it. No generation-time disk/provider I/O, new ticker, monitor worker or per-account poller is added.

An account is actually active while at least one request scope is in flight or for exactly one hour after its last completed scope. Every matching account qualifies, including concurrently used and round-robin subsets. The top two eligible priority candidates are independently likely-next; active takes precedence. Blocked, disabled and Retry-After guards remain authoritative.

Implementation review tightened replacement detection to cover attribute-based email and ID-token-only account/plan changes, not just explicit metadata. A changed ID token is parsed through the existing Codex parser only at the auth-update boundary, with a 64 KiB input cap. Stable account, plan, email and subject claims preserve ordinary refreshes despite changed issuance or signatures; missing, malformed or oversized changed tokens conservatively retire the cell. This is display-state invalidation, not token authorization, and introduces no decoding on attempts or monitor snapshots.

With usage Auto enabled, active fallback is 5–10 minutes, likely-next retains the selected target (20–40 minutes at the 30-minute default), and reserve fallback remains 20–28 hours. Manual-only still prevents all automatic usage reads. Active jitter is a stable pseudo-random draw from the opaque account key and latest real observation/attempt timestamp; it changes with that scheduling epoch, not with each viewer or process restart. This permits tightening an older long deadline without adding a new persisted schema or repeatedly redrawing it. A newly active account without any reading/attempt receives a bounded 5–10-minute deadline. Existing likely-next cold-start spreading remains unchanged.

Promotion can only tighten the existing automatic deadline. Demotion retains an already scheduled check, then the next genuine observation or attempt chooses the slower class interval; it never continually postpones overdue work. Fresh useful passive data postpones fallback from its actual capture time. Local views, request activity without a new quota reading, and partial/expired signals cannot manufacture freshness or continually push the deadline away. Every provider attempt retains the persisted duplicate-attempt floor, shared one-flight/10–60-second/six-per-minute budget and provider cooldowns. Reset inventory and its independent 60–120-second floor do not change. The strict v1 cache and rollback compatibility remain intact; runtime-only classification is not another durable authority.

Cross-client verification exposed a pre-existing admission/dispatch timing mismatch: variable synchronous persistence time could shorten the actual inter-dispatch gap. A deterministic one-second delayed-store test reproduced a nine-second gap behind a ten-second admission floor. The coordinator now corrects the common start/deadline and the unchanged automatic-reset draw from the post-persistence dispatch boundary, saving that correction before clearing the durable interrupted claim. One-flight ownership prevents a competing read during this correction. It adds at most one small common-control write per dispatched read, plus a changed reset companion for an automatic reset; unchanged local views still write nothing. No threshold is relaxed and no polling or waiting thread is introduced.

An interrupted cached claim cannot prove its actual dispatch time. On first recovery view, before retiring removed identities, it receives one conservative maximum shared-gap fence (60 seconds) and, for an interrupted reset, a maximum automatic-reset fence (120 seconds). Those deadlines are persisted once and ordinary views cannot slide them; recovery persistence failure blocks reads and preserves the claim. Completed attempts retain their corrected deadlines across normal restart without this fence. This deliberately trades one bounded delay after uncertain interruption for burst safety, using the existing strict v1 fields and reset companion rather than a second scheduler or schema migration.

Before release, require synthetic tests for actual transport versus selection/preparation/counting, long HTTP and streamed/WS requests, concurrent scopes, cancellation and duplicate cleanup, identity replacement and priority-only updates. Fake-clock scheduler tests cover exact class ranges, repeated views/restart, promotion/demotion, passive supersession, missing main windows, blocked identities, multiple clients and unchanged reset behavior. Retain the existing 128-account/4096-grant snapshot p95 <250 ms, zero idle writes, no new polling process/timer, Widget heartbeat <100 ms and close <11 seconds. Use the existing synthetic binary staging and real Widget verifiers; no live generation, reset or account experiment is QA. Full tests/builds, clean fork publication, installed Widget description update and independent live acceptance precede completion.

#### Item 4 release evidence — 2026-09-09 12:39 UTC

Session `01a077c3-26f5-7242-9d03-424a32cafc47`. Clean implementation `fdfa6ae8b867ecd31d81a9cc877cd478ab05d687` produces immutable SHA-256 `cb2e84421a4e26b98667beff0174be459b2c546108e3f614efe3d4b6cd2c1a91` with Go 1.26.2, trimpath, CGO disabled and `vcs.modified=false`. The final full Windows suite, real three-Widget integration and five-package Linux race subset pass. Final Windows snapshot evidence is 1.0071 ms p95 / 9.8715 ms maximum at 128 accounts, 4,096 grants and 1,000 views; serialized state is 732,024 bytes, sampled peak heap 5,610,640 bytes and idle writes zero. Native Windows race, physical-disk soak and human interaction are not claimed.

The exact binary passes synthetic held HTTP/stream activity, both assistant ACKs, passive capture, 30 unchanged zero-write views, coalesced persistence and abrupt restart. Its active deadline is 577.788071 seconds after the real capture; runtime-only activity resets independently while the cached observation and deadline survive. No external/provider monitoring call occurs. Five-model Fast metadata/outbound-tier normalization and authenticated/idempotent affinity checks pass. The cutover harness proves an external respawner can win the restart and an invalid candidate restores the baseline hash and healthy service. All scenarios use 48318/48319 and stop their owners.

Widget 0.38's accepted-source suite passes 549 tests, real-font minimum-size Settings, the canonical package, two temporary installer runs and three isolated native cold starts with 10.358-second bounded-I/O close. Existing repository build/install/staging/verifier owners cover this release; a separate release service or global tool would duplicate their responsibilities. Retained source/hash-bound task receipts are delivery inputs, not a new product control plane. The live router remains `56ff39cd` and Widget remains 0.37 until publication, fresh preflight/warning/action-time approval, guarded installation and independent acceptance complete.

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
the three-real-Widget-client integration enabled. Clean source `dbfb45e9cc7dee38431bb2caefff436c929434ad` builds with `vcs.modified=false` and SHA-256 `56ff39cdf1f0508f0af56279e2c6d8084f33357b70e3da5a4705478a895f16f9`.

Linux monitor race passes in 4.788 seconds. For 128 accounts/4096 grants/1000 snapshots, p95 is 1.008 ms, maximum 9.438 ms, serialized state 730,878 bytes, sampled peak heap 8,108,192 bytes and idle writes zero. No native Windows race or physical-disk soak is claimed. Synthetic binary capture/coalesced-flush/restart, five-model Fast forwarding, affinity authorization/idempotency and external-respawner/forced-rollback checks pass on isolated 48318/48319. Real-account requests are zero and all test owners exit.

Widget 0.33 is independently installed with both automatic lanes still off and prior settings/history preserved. Publication and a new action-time-approved protected-router activation remain required; staging is not live deployment. Preserve current `8e10e300` as the hash-locked rollback baseline, not its older predecessor. This item does not change usage classification or any later queued presentation feature.

### Item 1 publication and activation boundary

Implementation `dbfb45e9` plus staging docs `5bf3f376` are pushed with exact fork parity and clean source. Widget `e97e567` is pushed and 0.33 independently installed. The fixed candidate `56ff39cd` has a new hash-matching current `8e10e300` rollback; immediate read-only preflight reports ready with exact live owner/health. These checks do not replace action-time approval.

Telegram request 882 was delivered after the restart impact warning, but its 120-second acknowledgment wait timed out. No new approval or activation occurred. The prior release's approval is spent; do not replay its cutover or send another copy of 882 automatically. Before execution, obtain explicit approval for one brief protected 48317 restart and recheck candidate/live/rollback identity and health. Then prove independent installed health and Widget reconnection, publish the final evidence and send the single item completion notice. No later queue item starts before that boundary is completed.

### Item 1 live acceptance — 2026-09-09 01:02 -04:00

The pending gate above is historical. Ler requested automatic usage and reset inventory and replied "And yeah, thanks" directly to the brief-restart question. The assistant explicitly stated its interpretation of that assent and repeated the interruption warning before control. A fresh exact-volume gate and hash-locked preflight passed; the unchanged `dbfb45e9` candidate was activated by the existing deployer. Canonical SHA-256 remains `56ff39cdf1f0508f0af56279e2c6d8084f33357b70e3da5a4705478a895f16f9`, PID 1400236, HTTP 200 after deployment-parent exit. Immutable source tag: `ler-live-20260909-dbfb45e9`. The retained rollback matches `8e10e3005ec82369942711e75b663a021450fee9c262d734b79ecc8a6253de13`; only the redundant displaced executable was removed.

The installed Widget 0.33 reconnected without a manual provider request. Its two explicitly requested automatic modes were then saved through the shipped preference owner and verified after independent minimized cold start: usage target 30 minutes, reset inventory Daily, common gap 10 seconds. The separate automatic-reset 60–120-second floor is now live. All ten account cards, unchanged prior history, empty action journal and unrelated preferences pass. No real provider test generation, reset redemption, OAuth, configuration, priority, explicit affinity clear, App Server or SSH mutation occurred. Subsequent scheduled monitoring is requested normal operation, not a synthetic test or proof of provider account health. The companion state must remain with the cache across future rollback/re-upgrade.

## Passive-first baseline delivery checklist

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
It restricts both listeners to 48318/48319, uses two synthetic assistant replies
(held HTTP bootstrap with quota headers and a held stream without new quota)
and a CONNECT-denying loopback proxy, verifies management
authentication/strict input, manual-only views and unchanged cache writes, then
waits for the real coalesced flush before an abrupt restart. It never loads real
auth or checks live accounts. The existing Fast matrix remains a separate gate.
