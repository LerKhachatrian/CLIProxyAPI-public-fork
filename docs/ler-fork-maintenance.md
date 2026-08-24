# Ler CLIProxy Fork Maintenance

This document is the durable operating contract for Ler's CLIProxyAPI fork. Read it before upstream synchronization, custom feature development, staging, deployment, or rollback.

## Repository roles

- `origin`: official `router-for-me/CLIProxyAPI`; fetch-only for Ler custom work.
- `ler-public-fork`: writable `LerKhachatrian/CLIProxyAPI-public-fork` remote.
- Upstream baseline: an exact fetched `origin/main` SHA or an exact release tag.
- Integration branch: latest chosen upstream baseline plus Ler's ordered customization commits.
- Feature branch: one bounded customization based on the integration branch.
- Deployment tag: immutable source anchor for every binary promoted to live port `48317`.

Never treat a floating branch name as deployment provenance. Record the exact source SHA, binary SHA-256, build command, timestamp, and rollback artifact.

## Current customization stack

Apply and test customizations in this order:

1. Targeted Codex account reauthentication.
   - Reauthenticates exactly one selected file-backed Codex OAuth account.
   - Verifies returned account identity before mutation.
   - Preserves approved operator metadata and runtime attributes.
   - Fails closed for changed, ambiguous, unsupported, or mismatched targets.
2. GPT-5.6 Fast-mode compatibility.
   - Advertises Fast/priority capability for supported GPT-5.6 models.
   - Includes synthesized `gpt-5.6-sol-ultrafast` capability metadata when the provider exposes that model.
   - Normalizes client `service_tier: "fast"` to upstream `"priority"`.
   - Preserves `"priority"` and omits default, auto, or unknown tiers.
   - Keeps unrelated custom models Fast-disabled.

Each customization requires focused regression tests. A clean merge without passing behavior tests is not acceptance.

## Why Desktop and iOS differed for Fast mode

Codex Desktop discovers slash commands from the remote host's advertised model capabilities. The older deployed fork classified GPT-5.6 models as custom and returned an empty `service_tiers` list, so Desktop hid `/fast`.

ChatGPT iOS Remote Control can attach `service_tier: "priority"` directly to a turn. The older router already preserved literal `priority`, so mobile Fast requests worked even while Desktop capability discovery was wrong.

The fork must therefore maintain both halves of the contract:

1. Correct model capability advertisement.
2. Correct request-tier normalization and forwarding.

Do not attempt to solve this through Codex Desktop binaries, SSH dispatchers, Scheduled Tasks, App Server lifecycle shims, Remote Control identity state, or profile databases.

## Upstream synchronization runbook

1. Require a clean worktree and list all linked worktrees without modifying them.
2. Fetch `origin` branches and tags.
3. Pin the chosen upstream SHA and latest release tag in the changelog.
4. Tag and push the currently deployed source as a rollback anchor.
5. Create a new integration branch from the chosen upstream SHA.
6. Port or cherry-pick customizations one at a time in documented order.
7. Resolve conflicts against upstream architecture; never restore obsolete files wholesale after upstream refactors.
8. Run focused tests after each customization.
9. Add new work as separate feature commits.
10. Run formatting, the complete test suite, compile verification, and a secret/diff review.
11. Build an immutable staging binary and validate it on port `48318` with an isolated Codex client/App Server.
12. Commit and push the accepted integration branch; require zero dirty files.
13. Obtain action-time approval before touching protected live port `48317`.
14. Preserve the live binary, deploy atomically, verify Desktop and iOS Remote Control, and roll back immediately if acceptance fails.

## Fast-mode acceptance matrix

| Case | Expected model metadata | Expected forwarded request |
|---|---|---|
| GPT-5.6 Sol Fast | `priority` / `fast` advertised | `service_tier: "priority"` |
| GPT-5.6 Terra Fast | `priority` / `fast` advertised | `service_tier: "priority"` |
| GPT-5.6 Luna Fast | `priority` / `fast` advertised | `service_tier: "priority"` |
| GPT-5.6 Sol-Ultrafast Fast | advertised only when exposed by provider | `service_tier: "priority"` |
| Supported model Standard | Fast remains discoverable | no forwarded priority tier |
| Unsupported custom model | Fast not advertised | no forwarded priority tier |

Validation must prove behavior from the model-list response through the translated outbound Codex request. UI visibility alone is insufficient.

## Official Fast-mode contract

As of 2026-08-23, the official OpenAI documentation states that Fast mode is available in the ChatGPT desktop app and supports GPT-5.6. Codex CLI persists the choice as `service_tier = "fast"` with `[features].fast_mode = true`. CLIProxyAPI must therefore expose compatible model metadata to Desktop and translate the client-facing `fast` tier to the provider-facing `priority` tier without changing Standard requests.

Source: [Speed | ChatGPT Learn](https://learn.chatgpt.com/docs/agent-configuration/speed)

Re-fetch the official page before future compatibility work because supported models, credit multipliers, and client behavior can change independently of this fork.

## Hash-locked live cutover

Use `scripts/deploy-live-router.ps1` for live promotion. It has two explicit modes:

- `-PreflightOnly` is read-only. It verifies the candidate, current live binary, rollback binary, exact port owner, executable path, and loopback health endpoint.
- `-Execute` performs the cutover and is allowed only after an impact warning and action-time approval for protected port `48317`.

The execute path handles the existing launcher race without changing SSH, Codex Desktop, Scheduled Tasks, App Servers, or launcher scripts:

1. Verify all expected SHA-256 values and current listener ownership.
2. Copy the candidate to a unique file beside the canonical executable.
3. Rename the still-running old executable aside and atomically place the candidate at the canonical path. If either rename fails, restore the old pathname before stopping anything.
4. Stop only the verified old PID.
5. Briefly allow an existing CLIProxy launcher to respawn the canonical candidate; otherwise start it directly.
6. Require a new PID, the canonical candidate hash, exact executable ownership, and HTTP health.
7. On any post-swap failure, stop only the verified canonical-path process, restore the old hash, restart it, and require restored health before reporting failure.

The rollback binary must exist and match the expected old live hash before preflight can report ready. The script never reads or prints live configuration or authentication contents; it receives the config pathname only as a process argument.

Prove changes to this mechanism with `test/live_router_cutover_staging.ps1`. The harness uses disposable loopback ports and synthetic configuration to exercise both:

- a competing external respawner winning the restart race;
- a deliberately invalid candidate causing automatic rollback to the baseline hash and healthy service.

After a successful live cutover, verify `/fast` in a fresh Codex Desktop thread in a CLIProxy workspace. Refresh only the affected CLIProxy App Server connections if Desktop retains stale model metadata; restart the full Desktop app only as a final fallback.

## Deployment safety

- Staging port: `48318`.
- Protected live port: `48317`.
- Never restart or replace the live router without an explicit impact warning and action-time approval.
- Run the hash-locked preflight immediately before requesting that approval, then do not rebuild, replace, or rename the approved candidate before execution.
- Prefer a non-disruptive model refresh after deployment. Restart CLIProxy App Servers only if cache evidence requires it and approval covers that interruption.
- Do not read or print token-bearing auth files, management keys, cookies, provider credentials, or live `config.yaml` contents.
- Do not modify SSH routes, App Server sockets, Scheduled Tasks, profile histories, SQLite state, or Remote Control identities for provider metadata changes.

## Version-control completion contract

Every completed implementation must have:

- focused regression tests;
- full applicable test and build evidence;
- an additive changelog entry with the current Codex session ID;
- exact upstream, source, and binary provenance;
- a pushed branch or tag on `ler-public-fork`;
- no generated build artifacts in Git;
- `git status --porcelain` returning no output.

If production promotion is deferred, document the staging result and exact remaining approval boundary. Do not describe staging success as a live deployment.
