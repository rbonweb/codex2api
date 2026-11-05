# Earliest 5-hour reset first

Scheduling mode: `reset_5h_first`. Where to set it: General settings → Traffic and scheduling → Scheduling policy → Scheduling mode → **Earliest 5-hour reset first**.

This is a new optional mode. The default remains `round_robin`, and it takes effect only when explicitly selected. No database schema migration is needed.

## Existing similar mechanisms

`premium5hUsageUrgencyBonusLocked` already gives a near-reset bonus to eligible premium plans: when less than 4 hours remain and the remaining quota exceeds 5%, the bonus is calculated from the time and the remaining ratio, up to 25 points. It is part of the scheduling score and does not guarantee that the account with the earliest reset is used first. The existing "remaining quota" and "sequential exhaustion" modes mainly sort by the Codex weekly usage ratio.

## New mode

1. Manual account scheduling priority is honored first, then the health tier.
2. Within the same tier, accounts are chosen by the observed Codex 5-hour window reset time, earliest first.
3. Only usage that is valid, non-negative and below 100%, and a reset time strictly later than now, can earn this priority. It also does not prioritize spending the separate credit balance.
4. Rate limiting, disabled, scheduling stopped, permission mismatch, model filtering, full concurrency or failed admission means the next candidates are tried; weekly limits and the existing gates continue to apply as configured.
5. Expired, missing or inapplicable times do not project a new cycle; the corresponding engine's existing fallback is used. The index engine re-sorts when a reset boundary arrives or a snapshot is updated, and normally does not re-sort the whole account pool on every request.
6. An API-key-restricted group uses its own cached order; a snapshot update refreshes existing members, and an account without permission cannot be inserted into the group.
7. The first account selection for a new session follows this mode, and affinity hashing must not bypass the reset order. Existing session bindings and active turns continue to follow the original continuation logic; recoverable switching after an account is exhausted is handled by the earlier quota-switch patch.
8. The index, the legacy scan, the actual legacy path in shadow mode, and the lazy-loading fallback all support this ordering. Spark continues to use its own separate window and is not subject to the account-level 5-hour ordering.

If A resets in 4 hours, B in 1 hour and C in 2 hours, and the three have the same priority, health and availability, the new selection order is **B → C → A**.

## What it does not provide, and limits

This mode does not create scheduled tasks, does not send activation or warm-up requests, does not assume that a new cycle is always produced every 5 hours, does not add weekly quota, and cannot guarantee that the three accounts stay available continuously without limit. Upstream snapshots are delayed, and actual quota exhaustion must still be judged from the existing gates and upstream responses. A bound session may keep using its original account, so this also does not mean that every message reselects the account with the earliest reset.
