# CLAUDE.md — Project Conventions for new-api

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## MANDATORY: Read AGENTS.md with the Read tool

Do not treat `@AGENTS.md` as loaded. Claude Code does not reliably inline that import.

Before any planning, coding, reviewing, or answering a project question, you MUST call the Read tool on the repo-root file `AGENTS.md` and wait for the full contents. This is the first action of every session and every new task.

Rules:

- Do not start from memory, summaries, or this file alone.
- Do not skip the Read because a previous turn mentioned AGENTS.md.
- Do not replace the Read with a grep, glob, or partial skim.
- After reading, follow every rule in `AGENTS.md` for the rest of the work.
- If the task touches `web/`, also Read `web/AGENTS.md` before editing frontend files.
- If the task touches billing as defined under **Billing rules (mandatory read gate)** in `AGENTS.md`, also Read `.agents/rules/billing.md` in full before planning or editing. Tasks outside that definition may skip it.

This file only adds commands and an architecture map; AGENTS.md is the source of truth for rules.

## Commands

Backend (three Go modules: root, `relaykit/`, `tokenkit/`; there is no `go.work`):

```bash
make test                                   # all three modules, GOWORK=off (what CI runs)
go test ./service/ -run TestName -count=1   # single test in the root module
cd relaykit && GOWORK=off go test ./...     # module-local; same for tokenkit
go vet ./... && go build ./...              # CI runs both in each module
go run main.go                              # API server (default SQLite one-api.db)
```

`main.go` embeds `web/dist` via `//go:embed`, so building/vetting the root `main` package needs the frontend built first (`make build-web`). `make test` skips the root `main` package for this reason.

Frontend (run inside `web/`, use Bun):

```bash
bun install
bun run dev            # or `make dev-web` from repo root (port 5173)
bun run typecheck      # `bun check -b`, run in CI
bun run test           # vitest, run in CI; single file: bunx vitest run path/to/file.test.ts
bun run lint           # oxlint
bun run format         # oxfmt via scripts/, preserves protected headers
bun run i18n:sync
```

Docker dev stack (Postgres + API): `make dev-api`, `make dev-api-rebuild`; `make reset-setup` resets the setup wizard state.

## Architecture

Request path for relay (`/v1/...`): `router/relay-router.go` → `middleware/` (`TokenAuth`, `distributor.go` picks the channel, rate limits) → `controller/relay.go` `Relay()` (retry loop, error rendering per relay format) → `relay/*_handler.go` per API shape → `relay/channel/<provider>/` adaptor (implements `relay/channel/adapter.go`) → upstream. Billing pre-consume/settlement lives in `relay/` + `service/` (see billing gate in AGENTS.md).

Layers: `router/` (route groups) → `controller/` (HTTP handlers) → `service/` (business logic) → `model/` (GORM models, cache, migrations in `model/main.go`). `setting/` holds runtime options loaded from the `options` table; `dto/` and `types/` hold shared request/error types; `constant/` holds context keys and channel types.

### Local fork: scheduling center (智能路由)

This repo is new-api plus local routing and supplier extensions (see the merge commit in `git log`). Key pieces:

- `pkg/scheduler/` — in-process engine keeping per "group × channel × model" stats (success rate, first-token latency, in-flight capacity) and computing dynamic weights. Stats are per process, not cluster-wide.
- `service/channel_select.go` (`SelectChannelForRequest`, `CacheGetRandomSatisfiedChannel`) and `service/scheduler_routing.go` / `service/scheduling_config.go` — bridge between new-api channel candidates and the scheduler. `controller/relay.go` calls `service.ObserveSchedulingResponse` / `EndSchedulingRequest` to feed outcomes back.
- Admin UI/API: `controller/scheduling.go`, `router/scheduling-router.go`, frontend route `/scheduling`.
- `SCHEDULING.md` documents the metrics, weight formula and operator workflow in detail (Chinese) — read it before changing scheduling behavior.
- Supplier features: `controller/supplier*.go`, `router/supplier-router.go`, `middleware/supplier_auth.go`; product plans in `design-plans/`.

`.local-tests/` holds ad-hoc local verification artifacts and logs, not part of the test suite.

## README changelog

The READMEs carry a dated changelog instead of a commit-hash marker. When a change is visible to people who use or operate the gateway, add an entry under today's date to `更新日志` in `README.md` and `README.zh_CN.md` (their SmartRouter parts are identical) and to `Changelog` in `README.en.md`, and update the "最近更新" / "Latest update" line near the top of each. Describe what changed for the user, not which files changed; internal refactors and test-only changes get no entry. Keep the upstream README text at the end of each file untouched.

## Open item: MySQL verification of error analysis (run after go-live)

Status: **OPEN**, recorded 2026-11-25 at the owner's request. Delete this section once both steps are done and the result is recorded.

The error analysis changes (`83b4503`, `db4fa2f`, `e5a5891`, `a50e83e` and later error-analysis commits) were verified on real SQLite and PostgreSQL 16.15 only. No MySQL instance was available, so the three-database rule in AGENTS.md is not met yet. Do not describe these changes as verified on MySQL until the steps below have been run.

1. **Run one analysis on the live MySQL deployment.** After the release is deployed, start an error analysis from the console over a real range (for example the last 7 days) and let it finish. Check that the task succeeds, the server log has no SQL error from `error_analysis`, and the request totals in the report match the usage log for the same range.
2. **Run the test suite on MySQL.** The tests drop and recreate their tables, so use a throwaway database, never the production one:

   ```bash
   TEST_ANALYSIS_MYSQL_DSN='user:password@tcp(127.0.0.1:3306)/analysis_test?charset=utf8mb4&parseTime=True&loc=Local' \
     go test -race -count=1 -p 1 -run 'ErrorAnalysis|TestAnalysis' ./service/
   ```

Points that differ on MySQL and deserve a look: request IDs compare case-insensitively under the default collation (the exact request lookup filters again in Go); upserts into `error_analysis_artifacts` use `ON DUPLICATE KEY UPDATE`; the look-ahead for later rows of a request uses `idx_logs_request_id` with up to 1,000 IDs per page; reports are assembled from per-day segments (`model/error_analysis_segments.go`), which count and take the newest ID of each day's log rows and merge artifacts with `task_id IN (...) ORDER BY name`. Run a second analysis the next day as well and check that it reports reused days.

Record the MySQL version, the commands and the results in the handoff, as AGENTS.md requires.
