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
