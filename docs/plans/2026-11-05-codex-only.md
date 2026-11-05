# Plan: reduce codex2api to ChatGPT/Codex accounts and OpenAI-format APIs only

Owner decision (2026-11-05): this fork serves only ChatGPT/Codex OAuth accounts through OpenAI-format APIs. Everything else is removed, not hidden.

## Remove

- Upstream account types: Claude Code OAuth and Claude API key (`UpstreamClaude`, `IsClaudeOAuth`, `IsClaudeAPIKey`, setup-token), Grok/xAI (`UpstreamGrok`, `IsGrokAPI`, `platform=xai`), Antigravity (Google; Gemini and Claude via Antigravity, `IsAntigravityAPI`), and third-party OpenAI-compatible relay accounts (`UpstreamOpenAIResponses`, base URL + sk- key).
- Client-facing non-OpenAI APIs: Anthropic Messages `/v1/messages` and `/v1/messages/count_tokens` (with and without `/v1`), Gemini `/v1beta/*`, Grok video `/v1/videos*`, and Grok image branches inside `/v1/images/*`.
- Their admin routes and pages, settings (`claude_config`, `grok_config`, `antigravity_*` reads and writes), env vars (`GROK_OAUTH_CLIENT_ID`, `ANTIGRAVITY_*`, `CLAUDE_DISABLE_CLI_VERSION_SYNC`, `CLAUDE_DEVICE_ID`), schedulers (`StartAntigravityQuotaScheduler`, Claude CLI version sync), pricing rows (Grok, Gemini, Claude, media billing), model catalog entries, usage-log branches, frontend pages (`ClaudeAccounts.tsx`, `GrokAccounts.tsx`, `AntigravityAccounts.tsx` + `pages/antigravity/*`, `GrokImportProgressHost`, provider tabs in `Accounts.tsx` / `Settings.tsx` / `APIKeys.tsx`), locale keys, docs (`docs/ANTIGRAVITY.md`, provider sections of `README*.md`, `docs/API.md`, `docs/CONFIGURATION.md`, `.env.example`), `cmd/claude_login/`, `grok-build-main/` (untracked), and every test that only covers removed code.
- `IsRelayStyle` and every relay/Grok/Antigravity/Claude branch in shared code: after removal every account is a Codex account, so the predicates and the "skip relay-style accounts" filters go away instead of being trimmed.

## Keep (must keep working)

- Codex OAuth and Agent-Identity accounts, Daybreak (OpenAI cyber access program on Codex accounts: `auth/daybreak.go`, `database/daybreak*.go`, `proxy/daybreak_*.go`, `admin/daybreak.go`, Daybreak badges). Do not delete by name match.
- `/v1/responses` (HTTP, WebSocket, compact, input_tokens), `/v1/chat/completions`, `/v1/images/generations`, `/v1/images/edits`, image jobs, `/v1/realtime`, `/v1/live`, `/v1/live/:call_id`, `/backend-api/codex/*`, `/v1/models` (including the `client_version` manifest), `/v1/alpha/search`, plus the same routes without the `/v1` prefix.
- Pool scheduling and affinity, API key limits and billing, usage logs, prompt filter, output guard, image studio, proxy pool, intelligence checks, `proxy/wsrelay/` (Codex WebSocket pool, not a relay).

## Data

- One idempotent data migration (keyed in the existing data-migrations table, both PostgreSQL and SQLite) deletes non-Codex rows from `accounts` (dependent rows go with `ON DELETE CASCADE`), resets `api_keys.upstream_channel` values `claude`, `grok`, `antigravity`, `openai_responses`/relay to `codex`, and resets `account_groups.channel` the same way.
- Stop creating the Grok tables (`ensureGrokStateSchema`) and drop them in the same migration (`DROP TABLE IF EXISTS`). Leave the provider columns in `system_settings` in place (they are `ADD COLUMN IF NOT EXISTS`); just stop reading and writing them.
- Recreate the scheduler outbox and Grok maintenance triggers (`database/scheduler_outbox.go`) without the removed credential keys.
- Remove provider-only data-migration functions only together with a stub that keeps their keys marked done, so old databases do not rerun or fail on them.
- The account loader must ignore an unknown `upstream_type` with one log line instead of failing, so a database that still holds an old row starts.

## Acceptance

- `go build ./...`, `go vet ./...` and `go test ./...` pass on the parent branch (the only allowed failure is `TestAcceptanceScenario24_RealPipelineTTFTAndCPUGates` on macOS, which fails with "process CPU timer unavailable" on the untouched tree too). Frontend `npm run build` passes in CI (local npm refuses the npmmirror lockfile URLs, so the CI run is the frontend gate).
- `grep -rniE 'grok|antigravity|claude|gemini|anthropic|xai' --include='*.go' --include='*.ts' --include='*.tsx'` outside `CHANGELOG.md`, pricing history and model-trace data returns only lines justified in the lead's report (for example the OpenAI "Claude" string in a prompt-filter fixture).
- Routes check: the removed routes return 404; every kept route still answers (unit-level router test listing registered routes, plus the existing endpoint tests).
- A database that contains Grok, Claude, Antigravity and relay accounts and API keys with those channels migrates cleanly on SQLite (test) and the gateway starts.
- Admin frontend: no provider tabs, pages, routes or settings remain; account list, API keys, settings and usage pages build and render.
- Docs, README, `.env.example` and `docs/CONFIGURATION.md` describe only what exists. CHANGELOG gets one Unreleased "Removed" entry.

## Branching

- Lead branch `feat/codex-only` from `origin/main` in `.worktrees/codex-only`. Slices branch from `feat/codex-only` in their own `.worktrees/<slug>` and merge back after QA and review pass. Shared files (`proxy/handler.go`, `auth/store.go`, `admin/handler.go`, `database/postgres.go`, `database/sqlite.go`, `frontend/src/pages/Accounts.tsx`, `Settings.tsx`, `APIKeys.tsx`, `types.ts`, `api.ts`, locales) are owned by one slice at a time, never by two in parallel.
- The audio endpoints are a separate topic on `feat/audio` and must not touch the files above except one route block in `proxy/handler.go` RegisterRoutes; the main session merges the two parents and resolves that conflict.
