package auth

import (
	"fmt"
	"net/url"
	"strings"
)

// AccountHrefCredentialKey is the account's bound "jump URL" (inside the credentials JSON),
// so the account page can open the account's console/usage page in the browser with one click. Stored in the existing
// credentials JSON; older databases can safely ignore this field. An empty value means not configured,
// the frontend falls back to the account's base_url (api-base) when opening it.
const AccountHrefCredentialKey = "account_href"

// NormalizeAccountHref validates and normalizes the jump URL: it accepts only absolute http/https addresses,
// strips trailing slashes. Empty input is valid (= clear the setting).
func NormalizeAccountHref(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", nil
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Hostname() == "" {
		return "", fmt.Errorf("account_href must be a full http/https URL")
	}
	// Only strip the trailing slash of the path; trailing slashes in the query / fragment are part of the parameter content,
	// and must be left alone (for example, the slash in ?redirect=/usage/ is meaningful).
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed.String(), nil
}
