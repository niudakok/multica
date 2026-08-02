# Repository Guidelines

This file provides guidance to AI agents when working with code in this repository.

> **Single source of truth:** This file is a concise pointer document.
> All authoritative architecture, coding rules, and conventions
> live in **CLAUDE.md** at the project root. Read that file first.
> Use `Makefile`, `package.json`, and `pnpm-workspace.yaml` as the
> source of truth for the full command list.
> For the local agent daemon, read **CLI_AND_DAEMON.md** (`multica` CLI,
> `multica daemon start/restart/stop`, `~/.multica/daemon.log`).

## Quick Reference

### Architecture

Go backend + monorepo frontend (pnpm workspaces + Turborepo) with shared packages.

- `server/` - Go backend (Chi router, sqlc, gorilla/websocket)
- `server/cmd/multica/` - the `multica` CLI, which also runs the local agent daemon
- `server/pkg/agent/` - one adapter file per supported AI CLI runtime (see Agent Runtimes below)
- `apps/web/` - Next.js frontend (App Router)
- `apps/desktop/` - Electron desktop app
- `packages/core/` - Headless business logic (Zustand stores, React Query hooks, API client)
- `packages/ui/` - Atomic UI components (shadcn/Base UI, zero business logic)
- `packages/views/` - Shared business pages/components
- `packages/tsconfig/` - Shared TypeScript config

### Agent Runtimes (daemon)

The daemon detects installed AI CLIs, registers them as agent runtimes, and executes tasks by spawning the CLI. Each runtime is one backend in `server/pkg/agent/` (`claude.go`, `codex.go`, `opencode.go`, `openclaw.go`, `qwen.go`, ...). Three integration styles exist:

- ACP (Agent Client Protocol, JSON-RPC over stdio): hermes, kimi, kiro, traecli, copilot, grok, qoder — driven via `acpDiscoveryProvider` / `discoverACPModels`
- stream-json NDJSON on stdout: opencode, openclaw
- flag-based / `-p` prompt style: claude, codex, qwen, cursor, etc.

**Adding a new runtime touches these in lockstep:** `SupportedTypes` + the `New()` switch in `server/pkg/agent/agent.go`, the `runtime_profile.protocol_family` CHECK constraint (own migration, `NOT VALID` like migrations 134/136/175/179/242), a model-discovery path, and `scripts/agent-cli-command-names.txt`. Agent tests must never resolve/execute a real installed CLI; real-agent smoke tests need the `agentintegration` build tag and `MULTICA_RUN_REAL_AGENT_SMOKE=1`. See CLAUDE.md "Testing" and `server/pkg/agent/agent.go` header comment.

### State Management (critical)

- **React Query** owns all server state (issues, members, agents, inbox, workspace list)
- **Zustand** owns client/view state (view filters, drafts, modals, desktop tab state); current workspace identity is route-driven and only mirrored for platform plumbing
- All Zustand stores live in `packages/core/` - never in `packages/views/` or app directories
- WS events update React Query for server data; store writes are only for clearing client-owned pointers with a single responder/self-event guard

### Package Boundaries (hard rules)

- `packages/core/` - zero react-dom, zero localStorage, zero process.env
- `packages/ui/` - zero `@multica/core` imports
- `packages/views/` - zero `next/*`, zero `react-router-dom`, use `NavigationAdapter` for routing
- `apps/web/platform/` - only place for Next.js APIs

### Database Migrations (hard rules)

- Never add database foreign keys or cascading actions. Enforce relationships and perform dependent cleanup explicitly in the application layer, using transactions when the operation must be atomic.
- Every index created by a migration, including unique indexes and indexes on new tables, must use `CREATE [UNIQUE] INDEX CONCURRENTLY`. Keep each concurrent index build in its own single-statement migration file.

### Commands

```bash
make dev              # Auto-setup + start everything
make daemon           # Restart local agent daemon (multica daemon restart --profile local)
make test             # Go tests (needs local PostgreSQL; runs migrations first)
make check            # Full verification pipeline (typecheck + TS tests + Go tests + E2E)
make build            # Build server/multica/migrate binaries into server/bin
make sqlc             # Regenerate sqlc code after SQL changes
pnpm typecheck        # TypeScript check
pnpm test             # TS unit tests (Vitest)
pnpm exec playwright test  # E2E
```

See CLAUDE.md for the authoritative rules and common commands.
