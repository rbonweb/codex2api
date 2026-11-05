# Upstream sync log

This fork tracks [james-6-23/codex2api](https://github.com/james-6-23/codex2api) (`upstream`, branch `main`). This file records the last upstream commit reviewed and what was taken from it, so the next sync only has to look at newer commits.

## How to check for new upstream commits

```bash
git remote add upstream https://github.com/james-6-23/codex2api.git   # once
git fetch upstream main
git log --oneline --no-merges <last reviewed commit>..upstream/main
```

Port only what applies to this fork: it is Codex (ChatGPT) only, English only, and keeps its own audio, release and deployment code. Skip upstream changes for Claude, Grok, Antigravity/Gemini, relay accounts, `/v1/messages`, `/v1beta`, `/v1/videos`, Chinese-only text and upstream release notes. Cherry-pick with `git cherry-pick -n`, translate any Chinese comments, and add a row below.

## Last reviewed

| Field | Value |
|---|---|
| Upstream commit | `9676bfe8983f5a193bda1e97fd502cd27362017e` ("docs(changelog): v3.1.4") |
| Upstream commit date | 2026-11-05 11:29 UTC |
| Reviewed on | 2026-11-05 |
| Fork point (first fork commit's upstream base) | `20e4b399` (upstream v3.1.3, 2026-11-04) |

## History

### 2026-11-05: upstream `20e4b399..9676bfe8` (v3.1.3 to v3.1.4)

| Upstream commit | Date (UTC) | Subject | Decision |
|---|---|---|---|
| `f22cbf4e` | 2026-11-05 02:30 | fix(images): omit unsupported reasoning effort in image requests | Taken. Fixes "Complexity 'medium' is not supported for this image model." on every image request. |
| `21b28aa4` | 2026-11-05 03:25 | docs(images): document request builder and ingress regression | Taken (code comments and test). |
| `42ffd35d` | 2026-11-04 11:01 | fix(models): account text allowlist no longer hides image models in /v1/models | Taken, adapted to the Codex-only model list, comments translated. |
| `e09111da` | 2026-11-04 05:18 | fix(frontend): detail sheet arrow keys no longer hijack input cursors and dialogs | Taken. |
| `c2a7b55b` | 2026-11-04 05:42 | refactor(frontend): move the sheet key guard into its own module with tests | Taken. |
| `e2efc16c` | 2026-11-04 05:55 | test(frontend): strict editable-element selector, contenteditable=false case | Taken. |
| `fb3e15f0` | 2026-11-04 11:43 | Initial plan | Skipped (empty commit). |
| `9676bfe8` | 2026-11-05 11:29 | docs(changelog): v3.1.4 | Skipped (upstream release notes; this fork has its own changelog). |
