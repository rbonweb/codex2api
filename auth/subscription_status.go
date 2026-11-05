package auth

import (
	"context"
	"log"
	"strconv"
	"strings"
	"time"
)

// Subscription status is split into two layers: business status (active / expiring_today / expired ...) is derived from the expiry time
// using the business timezone; sync status (confirmed / pending / failed ...) describes the result of the latest query
// and data freshness. The two are stored separately, so the last known business status is kept when a query fails.
//
// Metadata lives in the account credentials key-value entries, so no table migration is needed.

// Subscription metadata credentials keys.
const (
	SubscriptionCheckedAtCredentialKey         = "subscription_checked_at"
	SubscriptionSyncStateCredentialKey         = "subscription_sync_state"
	SubscriptionSourceCredentialKey            = "subscription_source"
	SubscriptionErrorCredentialKey             = "subscription_error"
	SubscriptionLastKnownStatusCredentialKey   = "subscription_last_known_status"
	SubscriptionRenewalDetectedAtCredentialKey = "subscription_renewal_detected_at"
	SubscriptionWillRenewCredentialKey         = "subscription_will_renew"
	SubscriptionGraceUntilCredentialKey        = "subscription_grace_until"
)

// Sync status.
const (
	// SubscriptionSyncConfirmed means the latest query succeeded and the business status has been confirmed by the subscription provider.
	SubscriptionSyncConfirmed = "confirmed"
	// SubscriptionSyncPending means a renewal was observed (a paid plan is still in use and the old expiry time has passed),
	// or the user has started a refresh and the authoritative expiry time has not been received yet.
	SubscriptionSyncPending = "pending"
	// SubscriptionSyncFailed means the latest query failed, but the last known business status is still available.
	SubscriptionSyncFailed = "failed"
	// SubscriptionSyncUnknown means no query has ever succeeded, or no usable status exists.
	SubscriptionSyncUnknown = "unknown"
	// SubscriptionSyncUnsupported means the subscription provider has no subscription record for this workspace (k12/edu and similar
	// education/team plans), so only the expiry time in the login token can be relied on.
	SubscriptionSyncUnsupported = "unsupported"
)

// Expiry time source.
const (
	SubscriptionSourceJWT         = "jwt"
	SubscriptionSourcePlanHeader  = "plan_header"
	SubscriptionSourceProviderAPI = "provider_api"
)

// Business status.
const (
	SubscriptionStatusActive        = "active"
	SubscriptionStatusExpiringToday = "expiring_today"
	SubscriptionStatusExpired       = "expired"
	SubscriptionStatusGracePeriod   = "grace_period"
	SubscriptionStatusUnknown       = "unknown"
)

// Auto-renewal status: four values, so that false does not mean both off and unsupported.
const (
	SubscriptionAutoRenewEnabled     = "enabled"
	SubscriptionAutoRenewDisabled    = "disabled"
	SubscriptionAutoRenewUnsupported = "unsupported"
	SubscriptionAutoRenewUnknown     = "unknown"
)

// SubscriptionMeta is the subscription sync metadata (persisted in credentials).
type SubscriptionMeta struct {
	// CheckedAt is the time of the latest subscription query (success or failure), also used for probe throttling.
	CheckedAt time.Time
	SyncState string
	Source    string
	// Error is the reason the latest query failed; cleared on success.
	Error string
	// LastKnownStatus is the business status confirmed most recently, kept after a failed query or after a stale value is cleared.
	LastKnownStatus string
	// RenewalDetectedAt is when a validity change (renewal) was last detected; it is event information only.
	RenewalDetectedAt time.Time
	// WillRenew is the auto-renewal status: enabled / disabled / unsupported / unknown.
	WillRenew string
	// GraceUntil is the end of the grace period (returned by the subscription provider); the zero value means there is no grace period.
	GraceUntil time.Time
}

// SubscriptionMetaFromCredentials reads the metadata from credentials; get is the function that reads a value by key.
func SubscriptionMetaFromCredentials(get func(string) string) SubscriptionMeta {
	if get == nil {
		return SubscriptionMeta{}
	}
	return SubscriptionMeta{
		CheckedAt:         parseSubscriptionTime(get(SubscriptionCheckedAtCredentialKey)),
		SyncState:         strings.TrimSpace(get(SubscriptionSyncStateCredentialKey)),
		Source:            strings.TrimSpace(get(SubscriptionSourceCredentialKey)),
		Error:             strings.TrimSpace(get(SubscriptionErrorCredentialKey)),
		LastKnownStatus:   strings.TrimSpace(get(SubscriptionLastKnownStatusCredentialKey)),
		RenewalDetectedAt: parseSubscriptionTime(get(SubscriptionRenewalDetectedAtCredentialKey)),
		WillRenew:         strings.TrimSpace(get(SubscriptionWillRenewCredentialKey)),
		GraceUntil:        parseSubscriptionTime(get(SubscriptionGraceUntilCredentialKey)),
	}
}

// CredentialUpdates builds the credentials updates to write to the database (all eight keys; empty values are written as empty strings so they can be cleared).
func (m SubscriptionMeta) CredentialUpdates() map[string]interface{} {
	return map[string]interface{}{
		SubscriptionCheckedAtCredentialKey:         formatSubscriptionTime(m.CheckedAt),
		SubscriptionSyncStateCredentialKey:         m.SyncState,
		SubscriptionSourceCredentialKey:            m.Source,
		SubscriptionErrorCredentialKey:             m.Error,
		SubscriptionLastKnownStatusCredentialKey:   m.LastKnownStatus,
		SubscriptionRenewalDetectedAtCredentialKey: formatSubscriptionTime(m.RenewalDetectedAt),
		SubscriptionWillRenewCredentialKey:         m.WillRenew,
		SubscriptionGraceUntilCredentialKey:        formatSubscriptionTime(m.GraceUntil),
	}
}

func parseSubscriptionTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t
	}
	if secs, err := strconv.ParseInt(raw, 10, 64); err == nil && secs > 0 {
		return time.Unix(secs, 0).UTC()
	}
	return time.Time{}
}

func formatSubscriptionTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// SubscriptionStatusView is the subscription status object sent to the admin panel and frontend.
type SubscriptionStatusView struct {
	BusinessStatus string `json:"business_status"`
	Plan           string `json:"plan,omitempty"`
	ExpiresAt      string `json:"expires_at,omitempty"`
	// DaysRemaining is the number of calendar days remaining in the business timezone (set when active; 0 for expiring_today).
	DaysRemaining int `json:"days_remaining"`
	// DaysOverdue is the number of calendar days overdue in the business timezone (≥1 for expired / grace_period).
	DaysOverdue       int    `json:"days_overdue"`
	LastKnownStatus   string `json:"last_known_status,omitempty"`
	AutoRenew         string `json:"auto_renew"`
	GraceUntil        string `json:"grace_until,omitempty"`
	LastCheckedAt     string `json:"last_checked_at,omitempty"`
	Source            string `json:"source,omitempty"`
	SyncState         string `json:"sync_state"`
	RenewalDetectedAt string `json:"renewal_detected_at,omitempty"`
	Error             string `json:"error,omitempty"`
	Timezone          string `json:"timezone"`
}

// SubscriptionPlanTracked reports whether the plan needs subscription status tracking:
// paid plans are always tracked; a free plan is tracked only when an expiry time is kept (a paid plan that expired and was downgraded to free,
// the expiry time is then the evidence of a "real expiry", so the expired display must not be dropped just because of the downgrade).
func SubscriptionPlanTracked(planType string, expiresAt time.Time) bool {
	plan := strings.ToLower(strings.TrimSpace(planType))
	switch plan {
	case "", "api":
		return false
	case "free":
		return !expiresAt.IsZero()
	}
	return true
}

// businessDate returns the calendar day in the business timezone (midnight of that day).
func businessDate(t time.Time, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.Local
	}
	y, m, d := t.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, loc)
}

// businessDayDiff computes the number of calendar days between two business days (later - earlier).
// It subtracts the date components instead of dividing by 24h, so a daylight saving switch day is neither under-counted nor over-counted.
func businessDayDiff(later, earlier time.Time) int {
	l := time.Date(later.Year(), later.Month(), later.Day(), 0, 0, 0, 0, time.UTC)
	e := time.Date(earlier.Year(), earlier.Month(), earlier.Day(), 0, 0, 0, 0, time.UTC)
	return int(l.Sub(e).Hours() / 24)
}

// ComputeSubscriptionBusinessStatus only computes the business status and day counts, for internal paths such as ClearStale
// to reuse when recording LastKnownStatus.
func ComputeSubscriptionBusinessStatus(expiresAt time.Time, graceUntil time.Time, now time.Time, loc *time.Location) (status string, daysRemaining int, daysOverdue int) {
	if expiresAt.IsZero() {
		return SubscriptionStatusUnknown, 0, 0
	}
	if loc == nil {
		loc = time.Local
	}
	today := businessDate(now, loc)
	expiryDay := businessDate(expiresAt, loc)
	if expiresAt.After(now) {
		diff := businessDayDiff(expiryDay, today)
		if diff <= 0 {
			return SubscriptionStatusExpiringToday, 0, 0
		}
		return SubscriptionStatusActive, diff, 0
	}
	overdue := businessDayDiff(today, expiryDay)
	if overdue < 1 {
		overdue = 1
	}
	if !graceUntil.IsZero() && graceUntil.After(now) {
		return SubscriptionStatusGracePeriod, 0, overdue
	}
	return SubscriptionStatusExpired, 0, overdue
}

// BuildSubscriptionStatusView assembles the subscription status object; plans that do not need tracking return nil.
func BuildSubscriptionStatusView(planType string, expiresAt time.Time, meta SubscriptionMeta, now time.Time, loc *time.Location) *SubscriptionStatusView {
	if !SubscriptionPlanTracked(planType, expiresAt) {
		return nil
	}
	if loc == nil {
		loc = time.Local
	}
	status, remaining, overdue := ComputeSubscriptionBusinessStatus(expiresAt, meta.GraceUntil, now, loc)

	syncState := meta.SyncState
	if syncState == "" {
		syncState = SubscriptionSyncUnknown
	}
	source := meta.Source
	if source == "" && !expiresAt.IsZero() {
		// Historical data: the expiry time can only have come from the login token.
		source = SubscriptionSourceJWT
	}
	autoRenew := SubscriptionAutoRenewUnknown
	switch strings.ToLower(strings.TrimSpace(meta.WillRenew)) {
	case "true", SubscriptionAutoRenewEnabled:
		autoRenew = SubscriptionAutoRenewEnabled
	case "false", SubscriptionAutoRenewDisabled:
		autoRenew = SubscriptionAutoRenewDisabled
	case SubscriptionAutoRenewUnsupported:
		autoRenew = SubscriptionAutoRenewUnsupported
	}
	if syncState == SubscriptionSyncUnsupported {
		autoRenew = SubscriptionAutoRenewUnsupported
	}

	return &SubscriptionStatusView{
		BusinessStatus:    status,
		Plan:              strings.ToLower(strings.TrimSpace(planType)),
		ExpiresAt:         formatSubscriptionTime(expiresAt),
		DaysRemaining:     remaining,
		DaysOverdue:       overdue,
		LastKnownStatus:   meta.LastKnownStatus,
		AutoRenew:         autoRenew,
		GraceUntil:        formatSubscriptionTime(meta.GraceUntil),
		LastCheckedAt:     formatSubscriptionTime(meta.CheckedAt),
		Source:            source,
		SyncState:         syncState,
		RenewalDetectedAt: formatSubscriptionTime(meta.RenewalDetectedAt),
		Error:             meta.Error,
		Timezone:          loc.String(),
	}
}

// SubscriptionMeta returns a snapshot of the account's current subscription metadata.
func (a *Account) SubscriptionMetaSnapshot() SubscriptionMeta {
	if a == nil {
		return SubscriptionMeta{}
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.subscriptionMeta
}

// SubscriptionStatusView assembles the account's subscription status object from the current time and the business timezone.
func (a *Account) SubscriptionStatusView(now time.Time) *SubscriptionStatusView {
	if a == nil {
		return nil
	}
	a.mu.RLock()
	plan, expiresAt, meta := a.PlanType, a.SubscriptionExpiresAt, a.subscriptionMeta
	a.mu.RUnlock()
	return BuildSubscriptionStatusView(plan, expiresAt, meta, now, time.Local)
}

// UpdateSubscriptionMeta atomically changes the account's subscription metadata and persists it (all eight keys).
// mutate runs under the account lock and may only change meta itself.
func (s *Store) UpdateSubscriptionMeta(acc *Account, mutate func(m *SubscriptionMeta)) {
	if s == nil || acc == nil || mutate == nil {
		return
	}
	acc.mu.Lock()
	mutate(&acc.subscriptionMeta)
	snapshot := acc.subscriptionMeta
	acc.mu.Unlock()
	s.persistSubscriptionMeta(acc.DBID, snapshot, nil)
}

// persistSubscriptionMeta writes the metadata (optionally with extra credentials keys) to the database; on failure it only logs.
func (s *Store) persistSubscriptionMeta(dbID int64, meta SubscriptionMeta, extra map[string]interface{}) {
	if s == nil || s.db == nil || dbID <= 0 {
		return
	}
	updates := meta.CredentialUpdates()
	for k, v := range extra {
		updates[k] = v
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.db.UpdateCredentials(ctx, dbID, updates); err != nil {
		log.Printf("[account %d] failed to persist subscription sync metadata: %v", dbID, err)
	}
}

// GetSubscriptionExpiresAt returns the subscription expiry time currently recorded for the account (the zero value means unknown).
func (a *Account) GetSubscriptionExpiresAt() time.Time {
	if a == nil {
		return time.Time{}
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.SubscriptionExpiresAt
}

// BeginSubscriptionSync tries to take the async subscription sync slot; it returns false when a sync is already in flight.
func (a *Account) BeginSubscriptionSync() bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.subscriptionSyncInFlight {
		return false
	}
	a.subscriptionSyncInFlight = true
	return true
}

// EndSubscriptionSync releases the async subscription sync slot.
func (a *Account) EndSubscriptionSync() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.subscriptionSyncInFlight = false
	a.mu.Unlock()
}
