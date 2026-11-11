# Upstream sync log

This fork tracks [james-6-23/codex2api](https://github.com/james-6-23/codex2api) (`upstream`, branch `main`). This file records the last upstream commit reviewed and what was taken from it, so the next sync only has to look at newer commits.

## How to check for new upstream commits

```bash
git remote add upstream https://github.com/james-6-23/codex2api.git   # once
git fetch upstream main --no-tags
git log --oneline --no-merges <last reviewed commit>..upstream/main
```

Port only what applies to this fork: it is Codex (ChatGPT) only, English only, and keeps its own audio, release and deployment code. Skip upstream changes for Claude, Grok, Antigravity/Gemini, relay accounts, `/v1/messages`, `/v1beta`, `/v1/videos`, Chinese-only text and upstream release notes. Cherry-pick with `git cherry-pick -n`, translate any Chinese comments, and add a row below.

Fetch with `--no-tags`: upstream and this fork both use `v3.1.x` tag names, for different commits.

When every upstream commit up to the new tip has a row, record that tip as merged:

```bash
git merge -s ours <upstream commit>
```

This merge changes no files. It only tells git that upstream has been reviewed up to that commit. GitHub then shows the fork as up to date with upstream, and the next comparison starts from that commit instead of the fork point, so changes that were already ported do not conflict again. Merge the pull request with a merge commit, not squash or rebase, or the record is lost.

GitHub's "Sync fork" and a plain `git merge upstream/main` take every upstream change, including the provider code this fork removed. Use the steps above to take new upstream commits.

## Last reviewed

| Field | Value |
|---|---|
| Upstream commit | `75ece5ac4df4910409d37ea84264a4ae113be849` ("Merge pull request #864 from yanhao98/fix/antigravity-identity-revalidation-recovery") |
| Upstream commit date | 2026-11-09 10:59 UTC |
| Reviewed on | 2026-11-11 |
| Recorded as merged | Yes, up to the commit above (`git merge -s ours`, 2026-11-11) |
| Fork point (first fork commit's upstream base) | `20e4b399` (upstream v3.1.3, 2026-11-04) |

## History

### 2026-11-11: upstream `9676bfe8..75ece5ac` (after v3.1.4)

| Upstream commit | Date (UTC) | Subject | Decision |
|---|---|---|---|
| `34b490f8` | 2026-11-06 02:15 | fix(usage): record request completion time in usage_logs.created_at | Taken, adapted: the usage log insert has 71 columns here (72 upstream, which also writes `video_seconds`). Comments translated. Tested on SQLite and PostgreSQL 18. |
| `24616381` | 2026-11-05 05:46 | fix(proxy): fix device stabilization overriding the forced Codex UA | Taken, comment translated. Added a regression test (upstream has none). |
| `0aa0dff6` | 2026-11-07 03:20 | fix(deps): upgrade compression dependencies to fix the failing vulnerability check | Taken. brotli v1.2.7 (`GO-2026-6888`) and klauspost/compress v1.20.2 (`GO-2026-6865`). Both advisories were reachable in this fork. |
| `ad6bfd7b` | 2026-11-07 15:13 | feat(intelligence): add per-source automatic check switches | Skipped. The switches choose between Codex, Claude, Grok and Antigravity accounts. This fork has Codex accounts only, and its check is already single-source. |
| `b68edde9` | 2026-11-07 15:42 | fix(intelligence): address source switch review findings | Skipped (follow-up to `ad6bfd7b`: Grok and Antigravity models and credentials). |
| `9093c1f5` | 2026-11-07 15:54 | fix(intelligence): preserve auth fences and native Grok failures | Skipped (follow-up to `ad6bfd7b`: Grok cooldown and Antigravity credential fence). |
| `1d9c1801` | 2026-11-05 12:13 | fix(antigravity): isolate Argon quota cooldowns from Gemini | Skipped (Antigravity). |
| `5772bacf` | 2026-11-05 12:27 | test(antigravity): cover Argon family-only cooldown admission | Skipped (Antigravity). |
| `231e2ea1` | 2026-11-05 12:41 | fix(antigravity): retain quota metadata family fallback | Skipped (Antigravity). |
| `461ef666` | 2026-11-09 00:58 | fix(antigravity): auto-recover identity revalidation quarantine and retry transient userinfo transport errors (#863) | Skipped (Antigravity). |
| `d5f911ae` | 2026-11-09 02:02 | fix(antigravity): address CodeRabbit review on transport retry scope and prior subject verification | Skipped (Antigravity). |
| `496a7136` | 2026-11-09 04:31 | fix(antigravity): preserve principal transition fence across repeated refreshes and exclude TLS cert verification errors | Skipped (Antigravity). |
| `32cc6ac8` | 2026-11-09 05:43 | fix(antigravity): move pending subject cleanup to ReplaceAccountCredentialsCAS and preserve transition fence on failure | Skipped (Antigravity). `ReplaceAccountCredentialsCAS` still exists here, but the change only handles an Antigravity credential key. |

The four merge commits in this range (`59ee1a30`, `0674a02e`, `b6a827f9`, `75ece5ac`) carry no changes of their own.

Before this sync, a merge of upstream into this fork conflicted in 29 files:

- 3 files that the 2026-11-05 sync changed. The image model fix in `proxy/scoped_models.go` and its paragraph in `docs/API.md` were ported by hand, so both sides had the same change in a different form. Both sides also added notes at the top of `CHANGELOG.md`.
- 12 Antigravity and Grok files that upstream changed and this fork deleted.
- 14 files that both sides edited at the same lines: the intelligence check (8 files: Claude removed here, Grok and Antigravity added upstream), Antigravity code in `auth/store.go` and `proxy/handler.go`, the usage log insert (`database/postgres.go` and its column count test), and `go.mod` with `go.sum` (audio dependencies here, version upgrades upstream).

Upstream changes to the removed provider files will conflict again at every sync. Keep those files deleted.

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
