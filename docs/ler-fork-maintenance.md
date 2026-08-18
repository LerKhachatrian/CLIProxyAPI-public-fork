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

## Deployment safety

- Staging port: `48318`.
- Protected live port: `48317`.
- Never restart or replace the live router without an explicit impact warning and action-time approval.
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
