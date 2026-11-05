# Intelligence check

Manage automatic or manual checks for fixed accounts at `/admin/intelligence-check`. Each run picks, in question-bank order, one enabled question, compares the final visible answer and the elapsed time with the administrator's configuration, and saves the result and a configuration snapshot. GPT/Codex accounts are supported. A check result describes performance under the current question and judging rules; it is not used to prove the upstream model's actual identity.

## Contents

- [Enable and configure](#enable-and-configure)
- [Results and history](#results-and-history)
- [Automatic actions](#automatic-actions)
- [Requests and proxies](#requests-and-proxies)
- [Storage and job execution](#storage-and-job-execution)
- [Development verification](#development-verification)

## Enable and configure

Automatic detection's master switch is off on first initialization, and existing accounts do not take part in detection. After saving the configuration and question bank, turn on the required accounts and the master switch. New accounts take part by default but still follow the master switch; this default can be changed. Manual checks use the saved configuration and do not require automatic detection to be turned on.

| Field | Default | Description |
| --- | --- | --- |
| `enabled` | `false` | Master switch for automatic detection |
| `gpt_model` | `gpt-6-astra` | Detection model; account capability is verified before sending |
| `reasoning_effort` | `high` | GPT reasoning effort |
| `judge_mode` | `answer` | `answer`, `latency` or `both` |
| `interval_seconds` / `retry_seconds` | 300 / 60 | Normal detection interval and retry interval after an error, range 1–86400 |
| `timeout_seconds` | 180 | Request timeout, range 1–3600 seconds |
| `concurrency` | 4 | Limit shared by manual and automatic detection, range 1–100 |
| `retention` | 200 | Number of terminal history entries kept per account, range 1–10000 |
| `new_accounts_enabled` | `true` | Affects only accounts created later |
| `action` | `none` | `none`, `pause` or `switch_group` |
| `degrade_threshold` / `recover_threshold` | 2 / 1 | Consecutive failures and recovery thresholds, range 1–1000 |
| `target_group_id` | 0 | When switching groups, must name a valid group compatible with the account's channel |

Each question has `id`, `name`, `prompt`, `expected_answer`, `match_mode`, `max_duration_ms`, `enabled`; at most 100 questions. Before turning on automatic detection or submitting a manual job, there must be at least one enabled question. The built-in candy-pairing sample uses the expected answer `21`, and the administrator can edit or replace it.

Answers support full match `exact`, contains match `contains`, and Go regex match `regex`. Before judging, Unicode NFKC normalization is applied, and leading and trailing whitespace and a single-level code fence wrapping the entire answer are removed. A latency judgment requires a complete successful response whose elapsed time is strictly less than the question's threshold; elapsed time does not include local queue time. Configuration and the question bank are saved together; the API uses `revision` to check concurrent edits, and conflicts return 409.

## Results and history

| Status | Display | Meaning |
| --- | --- | --- |
| `queued` / `running` | Waiting to run / Checking | No result yet |
| `normal` | Green `#22c55e` | Meets the selected judging rule |
| `degraded` | Red `#ef4444` | Complete response, but the answer or latency did not pass |
| `error` | Dark red `#991b1b` | Request failed, timed out, or no successful terminal event was received; it cannot be judged as a wrong answer |
| `interrupted` | Interrupted | Service stopped or the execution lease became invalid; not counted in judging |

The account table, cards and details directly share the `AccountStatusBar` component of the business health bar; each cell of the check bar represents one check and shows the latest 20 terminal results. The pass rate counts only normal / (normal + not passed), and shows `--` when there is no scorable result. Hover to see the summary; click a colored block to open that check's details.

After the status badge, compact check and history icons are shown; a spinner is shown while waiting and running. The history dialog supports paging, and keeps the question, judging configuration, final output, errors and action records, and can toggle automatic detection for this account. When automatic detection is turned off for an account, its check bar is hidden, but its history can still be viewed and manual checks can still be run. The current page's summaries are polled in batches, and polling pauses when the page is hidden; summary queries do not load full response bodies.

## Automatic actions

Only `normal` and `degraded` change the consecutive count; request errors and interruptions neither increase nor break the count. When the threshold is reached, business scheduling can be paused or the account can be switched to another group; the account keeps taking part in enabled checks to observe recovery.

- Pausing uses a separate check-pause flag. Recovery only lifts the pause owned by this feature, and does not enable an account that was manually disabled or clear other states such as authorization or quota. After the pause policy is turned off, existing pauses are not lifted automatically.
- Switching groups saves the original complete set of groups, and recovery restores the original groups that still exist and whose channel has not changed. Turning off the switch-group policy, existing group switches can still be restored by the recovery threshold.
- Manually changing an account's scheduling switch cancels the ownership of the old action and resets the consecutive count. Manually changing its group cancels the old group restore eligibility; this includes writing the same value and batch operations. An independent check pause is not lifted merely by changing the group.
- If the configuration or manual state changes while a job is running, the old job may keep its history, but it cannot overwrite the new state with the old configuration.

## Requests and proxies

Checks run on fixed accounts, reusing the existing Codex/Responses request executor and the account identity configuration; there is no account-switch fallback, no quality-test HTML instructions are injected, and the administrator's browser identity is not forwarded upstream. A success judgment requires a successful protocol terminal event and uses only the final visible answer; it does not take the answer from reasoning content or tool arguments.

Checks use only the account's bound proxy; when none is bound, a direct connection is explicitly used, and when binding fails the error is recorded and it does not fall back to a direct connection. Checks do not inherit the group proxy, the global proxy pool, the global single proxy, Resin or the environment proxy. Client identity, fingerprint and time zone are still resolved from the original account/global configuration. The ordinary business outbound rules stay unchanged.

Checks produce upstream usage; logs use the internal source `intelligence_check` and are not attributed to a user API key. Errors and output in history go through the existing credential redaction, and the execution lease token is not returned by the admin API.

## Storage and job execution

On initialization, SQLite and PostgreSQL add `intelligence_check_settings`, `intelligence_check_accounts` and `intelligence_check_runs` tables with their indexes, and a trigger that initializes new accounts. The migration can be run repeatedly, and existing accounts have checks turned off by default.

Database transaction constraints share check concurrency and allow at most one active job per account, and use leases and executor tokens to reject stale submissions. Expired runs are marked interrupted; checks also respect this instance's hard per-account concurrency limit and are re-queued when busy. The configuration snapshot is refreshed when a job is claimed; results and automatic actions are committed in the same transaction; scheduling changes are synced through the existing outbox. History is cleaned up per account; the existing quality-test tables and endpoints are kept.

For the API list and request format, see [API docs](API.md#intelligence-check).

## Development verification

```bash
go test -race -count=1 ./internal/intelligencecheck ./database ./auth ./admin ./proxy ./proxy/wsrelay -run 'Intelligence|AccountBound|BoundEgress|UpstreamTrace'
npm --prefix frontend test
npm --prefix frontend run typecheck
npm --prefix frontend run test:intelligence
npm --prefix frontend run build
```

Browser cases use simulated endpoints and require Playwright Chromium to be installed first. The PostgreSQL behavior set uses an isolated schema; after setting `CODEX2API_TEST_POSTGRES_DSN`, run `go test -race -count=1 ./database -run '^TestIntelligenceCheckPostgres$'`.
