# Adaptive intelligence check

Adaptive detection is off by default; the upgrade does not change the existing automatic detection switch, recovery thresholds or fixed frequency. It can be enabled under "Intelligence check → Frequency and resources", and the configuration uses the existing settings API and JSON storage, with no new business table.

## Configuration

| JSON field | Default | Meaning |
| --- | --- | --- |
| `adaptive_enabled` | `false` | Turns on adaptive detection |
| `pause_initial_seconds` | `1800` | Initial interval after an automatic pause |
| `pause_max_seconds` | `172800` | Maximum interval for automatic pause probing; can be set up to 30 days; not tied to the normal interval |
| `recovery_initial_seconds` | `300` | Confirmation interval after the first pass |
| `recovery_min_seconds` | `120` | Lower bound of the confirmation interval |

The initial pause interval must not exceed the maximum; the recovery confirmation lower bound must not exceed the initial confirmation interval, and the initial confirmation interval must not exceed the initial pause interval. The existing `interval_seconds`, `retry_seconds`, `degrade_threshold` and `recover_threshold` continue to control normal detection, retry after the first error, pausing and recovery thresholds. Enabling this feature does not automatically rewrite the thresholds.

## Scheduling rules

- Normal accounts: detected at the normal interval. After a request error or interruption, the retry interval starts doubling, up to the normal interval (if the retry interval itself is longer, it is kept). After a detection completes successfully, the error-retry backoff is cleared.
- Quality automatic pause: the first pause uses the initial pause interval; from then on, every result that does not pass doubles the interval, up to the configured maximum. A request error is evidence of unavailability and is not treated as confirmed intelligence degradation. This policy applies only to quality pauses and does not change the recovery ownership semantics of group switching.
- Recovery confirmation: one normal result enters confirmation, and the account stays paused. The confirmation interval halves each time until it reaches the lower bound; when the lower bound is at least one minute, it is rounded down to whole minutes, so the default is 5 minutes, then 2 minutes, then 2 minutes. Recovery happens only when the existing consecutive-pass threshold is reached.
- Failed confirmation: an error, interruption or failed detection resets the confirmation count to zero and returns to the previous pause interval, without an extra doubling this time. Only when the next regular recovery probe fails again does the interval double again.
- Historical backoff: the original interval is kept when scheduling recovers. Each normal detection after that halves the historical interval, and it is cleared once it reaches the initial interval. If the pause threshold is reached again, the remaining historical interval is reused. The historical interval does not affect the normal detection frequency of healthy accounts.
- Jitter: the actual wait is randomly chosen as 90%–100% of the nominal interval, and never exceeds the configured maximum. The next time is saved to the database when detection completes, so restarts and polling do not draw it again.
- Credential revocation: explicit `token_revoked`, `account_deactivated`, `workspace_deactivated` or `organization_deactivated` errors in a detection response pause automatic detection for that credential generation. Once the credential is updated and the generation changes, probing is allowed again; manual detection is still allowed, and a success lifts the block. An ordinary 401/expiry or overload does not cause a permanent block on this basis.

All state is persisted in `state_json`, and job lease expiry also uses the same backoff algorithm. Configuration revisions, manual actions and credential generations isolate old detection results; a legacy in-flight snapshot without a generation keeps only its history. Manually enabling or disabling business scheduling clears historical backoff, but does not replace credential repair. Turning off automatic detection does not lift a quality pause.

Saving the configuration resets the existing consecutive-decision counts and keeps and limits the historical backoff; shortening the maximum brings far-future detections forward. Switching the adaptive toggle reschedules the next time under the new policy. Disabling the adaptive policy returns to the original fixed frequency and clears the credential blocks saved by that policy.

## Suggested example

With a normal interval of 1800 seconds, a retry of 300 seconds after an error, a pause after 3 consecutive failures and recovery after 3 consecutive passes, and the defaults in the table above:

- Pause probing: 30 minutes → 1 → 2 → 4 → 8 → 16 → 32 → 48 hours, then it stays at the maximum.
- Recovery confirmation: first normal result → normal about 5 minutes later → normal about 2 minutes later → business recovers.
- A long interval delays discovery of recovery; the 48-hour maximum means the worst case is nearly two days before recovery is discovered. The account's "Detect now" can be used to confirm early, but it does not bypass the consecutive-pass threshold.

The account detection bar and the history dialog show the current stage, confirmation progress and the persisted next detection time. A credential block shows "Waiting for credential repair".

## Verification

State machine tests cover backoff across days, variable maximums, failed confirmation, recurrence memory and legacy policies; database tests verify SQLite/PostgreSQL persistence through real job enqueue, claim, completion and lease expiry, and credential generation isolation; browser regression uses simulated endpoints and does not call a real model.
