# Documentation index

Documentation for codex2api, the ChatGPT/Codex OpenAI-compatible gateway. Start with the [project README](../README.md).

## Getting started

- [API.md](API.md): All HTTP endpoints, request and response formats, error codes, and authentication.
- [USAGE.md](USAGE.md): The admin guide at `/admin/docs` with client setup, a first request, and copyable examples.
- [DEPLOYMENT.md](DEPLOYMENT.md): Docker Compose and Render deployment, with external PostgreSQL or SQLite.
- [CONFIGURATION.md](CONFIGURATION.md): Environment variables, system settings, and how they take precedence.

## Operations

- [ARCHITECTURE.md](ARCHITECTURE.md): System architecture, core components, data flow, scheduler, and storage.
- [TROUBLESHOOTING.md](TROUBLESHOOTING.md): Diagnosis for startup, database, account pool, request, performance, and proxy problems.
- [INTELLIGENCE_CHECK.md](INTELLIGENCE_CHECK.md): Automatic and manual checks that compare a pinned account's answers with a question bank.
- [INTELLIGENCE_BACKOFF.md](INTELLIGENCE_BACKOFF.md): Adaptive check frequency for intelligence checks, off by default.
- [SCHEDULED_5H_ACTIVATION.md](SCHEDULED_5H_ACTIVATION.md): Scheduled account activation from the account test dialog.
- [RESET_5H_PRIORITY.md](RESET_5H_PRIORITY.md): The `reset_5h_first` scheduling mode, which favours accounts whose 5-hour window resets soon.
- [proxy_pool.md](proxy_pool.md): Enhanced proxy pool management.
- [proxy-risk-scoring.md](proxy-risk-scoring.md): Optional IP fraud-risk scoring for pool proxies, used for review only.

## Codex client compatibility

- [CODEX_CLIENT_VERSIONS.md](CODEX_CLIENT_VERSIONS.md): How app versions are paired with their bundled CLI versions.
- [CODEX_ATTESTATION.md](CODEX_ATTESTATION.md): The shared `X-Oai-Attestation` rules for outbound Codex requests.
- [CODEX_MULTI_AGENT_V2_COMPAT.md](CODEX_MULTI_AGENT_V2_COMPAT.md): The optional compatibility layer for Codex Multi-Agent v2 with third-party sub-agents.
- [CODEX_SESSION_IDENTITY.md](CODEX_SESSION_IDENTITY.md): The session identity convergence mode for account fingerprints.
- [RESPONSES_COMPATIBILITY.md](RESPONSES_COMPATIBILITY.md): What the Responses endpoint supports, and where compatibility ends.

## Features

- [IMAGE_JOB_QUEUE.md](IMAGE_JOB_QUEUE.md): The persistent image job queue and its worker settings.
- [IMAGE_RESULT_STORAGE.md](IMAGE_RESULT_STORAGE.md): Storage options for image job results.
- [IMAGE_STORAGE_MAINTENANCE.md](IMAGE_STORAGE_MAINTENANCE.md): Image library performance and retention rules.
- [prompt-filter-hardening.md](prompt-filter-hardening.md): The opt-in strict terminal mode for Prompt Filter rules.
- [prompt-filter-live-test-report.md](prompt-filter-live-test-report.md): How to validate Prompt Filter without production data.
- [newapi-audit-integration.md](newapi-audit-integration.md): The optional NewAPI identity and execution adapter.

## Development

- [CONTRIBUTING.md](CONTRIBUTING.md): Development setup, code and commit conventions, the pull request flow, and test requirements.
- [plans/2026-11-05-codex-only.md](plans/2026-11-05-codex-only.md): The plan to limit the gateway to ChatGPT/Codex accounts and OpenAI-format APIs.
- [../AGENT_IDENTITY_IMPORT.md](../AGENT_IDENTITY_IMPORT.md): The import API for Codex Agent Identity accounts.
- [../DESIGN.md](../DESIGN.md): UI constraints that every frontend change must follow.
