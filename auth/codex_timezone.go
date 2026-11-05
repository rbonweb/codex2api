package auth

import (
	"strings"
	"time"
)

// AccountTimezoneCredentialKey is the storage key for the account's bound timezone in the credentials. It means "the timezone this account presents
// to the upstream"; empty = not bound, and the downstream client's timezone passes through unchanged.
const AccountTimezoneCredentialKey = "timezone"

// NormalizeAccountTimezone normalizes the account's bound timezone: an empty string, "Local", and IANA names that cannot be loaded
// all return empty (= not bound). It is better to fall back silently than let one bad value break the whole account's requests.
func NormalizeAccountTimezone(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || strings.EqualFold(value, "Local") {
		return ""
	}
	if _, err := time.LoadLocation(value); err != nil {
		return ""
	}
	return value
}

// EffectiveCodexTimezone returns the bound timezone in effect on the official Codex outbound path.
func (a *Account) EffectiveCodexTimezone() string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return strings.TrimSpace(a.Timezone)
}

// ApplyAccountTimezone syncs a bound timezone changed in the admin panel to the runtime account,
// so it takes effect without waiting for the next full reload.
func (s *Store) ApplyAccountTimezone(dbID int64, timezone string) bool {
	acc := s.FindByID(dbID)
	if acc == nil {
		return false
	}
	acc.mu.Lock()
	acc.Timezone = NormalizeAccountTimezone(timezone)
	acc.mu.Unlock()
	return true
}
