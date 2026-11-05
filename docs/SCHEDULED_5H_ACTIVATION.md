# Scheduled account activation

Entry: Account → Test connection → "Scheduled activation" to the right of "Model fingerprint check". This entry opens a separate secondary dialog.

## Configuration

- Choose the test model and test content for the current Codex/Responses account.
- Add and remove multiple time points separately for weekdays (Monday to Friday) and weekends (Saturday and Sunday).
- Times are uniformly in `Asia/Shanghai` (Beijing time).
- Each plan can be turned on or off independently; accounts without a configured plan are off by default.
- Clicking save only saves the plan and does not send a request immediately. The model, content and time points are persisted in the current account's `scheduled_connection_tests` configuration.

The configuration API is provided under admin authentication:

- `GET /api/admin/accounts/:id/scheduled-tests`: the configuration, time zone and the most recent ten execution records.
- `PUT /api/admin/accounts/:id/scheduled-tests`: saves the complete configuration.

```json
{
  "enabled": true,
  "model": "the test model available for the selected account",
  "content": "hi, reply ok",
  "weekday_times": ["05:40", "10:40", "15:40"],
  "weekend_times": ["09:00", "14:00", "19:00"]
}
```

Plans are configured per account, and no timetable is created automatically by default.

## Execution

The gateway process checks plans every minute. Closing the browser or the Codex desktop app does not affect execution. Automatic tests reuse the existing test-connection handler, bind one account at a time, and do not go through the polling, remaining-quota, near-reset-first or quota-based account-switching flows. Codex OAuth scheduled requests explicitly use a separate HTTP path and request `none` reasoning effort.

Each time point runs only once, and failures are not retried or switched to another account. Accounts that are already on a timer, locked, or out of quota can still be tested under their enabled plan; after an account is disabled or deleted, or its plan is turned off, later tasks stop. The plan is re-read before execution so that time points deleted while the run was queued are not executed. Slots missed while the service was offline are not made up.

Execution requests really consume upstream quota. The plan itself cannot guarantee that the upstream will start or reset the five-hour window; records follow the visible usage status after the response.

## Records and recovery

Each `(account ID, Shanghai date-time slot)` pair has a unique persisted claim that prevents duplicate sends from concurrent instances or after a service restart. Results are recorded in the `scheduled_connection_test:` namespace of the existing `maintenance_jobs` table; the JSON summary is stored in the `last_error` field (a successful record does not mean there was an error).

The summary includes the scheduled time, the actual execution time, the account ID, the model, the success/failure/skipped status, and the latest visible five-hour reset time. The full text of the test response and any credentials are not stored. The page shows the most recent ten records; the test's existing usage log additionally carries a `scheduled_connection_test` internal reason.

If the service exits unexpectedly after the claim but before the result is recorded, the slot shows "result unconfirmed" and is not automatically resent, to prevent duplicate requests. Disabling a plan cannot revoke a request that has already been sent upstream.
