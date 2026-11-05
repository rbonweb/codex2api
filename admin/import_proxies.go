package admin

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/codex2api/security"
)

// maxImportedProxies limits how many proxies a single import can register in the proxy pool. The import file is external input,
// and without a limit a malformed file could flood the proxies table.
const maxImportedProxies = 500

// proxyOverwritePolicy decides what to do with the proxy an existing account is already bound to when an upsert hits that account.
type proxyOverwritePolicy bool

const (
	// overwriteAccountProxy overwrites the existing binding with the incoming proxy.
	overwriteAccountProxy proxyOverwritePolicy = false
	// preserveAccountProxy keeps the existing binding and only fills in a proxy when the account has none.
	preserveAccountProxy proxyOverwritePolicy = true
)

// importProxyBinding is one pending "account → proxy" binding. The import entry structures differ between channels
// (for example Codex's importToken), so each one is projected into this shape and then shares one registration logic.
type importProxyBinding struct {
	// url is normalised in place by registerImportedProxyBindings: an invalid value is cleared, and the caller uses this to
	// fall back to the proxy filled in on the form.
	url string
	// enabled is the enabled state exported by the source; nil means the file did not include it (an old file). It is only used for alert counting.
	enabled *bool
}

// importProxyOutcome summarises the proxy registration result of one import, for SSE counts and alerts.
type importProxyOutcome struct {
	inserted int
	skipped  int
	// unusable holds the proxies that are still not in the enabled set after registration. Either the source marked them disabled, or
	// this machine already had the same URL disabled or failing its test; ON CONFLICT DO NOTHING will not
	// revive it. When the proxy pool is enabled, accounts bound to these proxies are never schedulable.
	unusable int
	warnings []string
}

// warning combines the alerts into one SSE message; it returns an empty string when there are no alerts.
func (o importProxyOutcome) warning() string {
	return strings.Join(o.warnings, "; ")
}

// importedProxyLabel tags this batch of proxies with a timestamp label, so they can be filtered and cleaned up by batch later on the proxy management page.
func importedProxyLabel() string {
	return "imported-" + time.Now().UTC().Format("20060102-1504")
}

// registerImportedProxies is the entry point for Codex import entries. It takes the three proxy fields on importToken
// and projects them into a generic binding, passes that to registerImportedProxyBindings, and writes the normalised result back to the entry.
func (h *Handler) registerImportedProxies(ctx context.Context, tokens []importToken) (importProxyOutcome, error) {
	bindings := make([]importProxyBinding, len(tokens))
	for i := range tokens {
		bindings[i] = importProxyBinding{url: tokens[i].proxyURL, enabled: tokens[i].proxyEnabled}
	}
	outcome, err := h.registerImportedProxyBindings(ctx, bindings)
	for i := range tokens {
		tokens[i].proxyURL = bindings[i].url
	}
	return outcome, err
}

// registerImportedProxyBindings writes the proxies carried in the import file into the proxy table and syncs them to the in-memory proxy pool,
// and normalises the proxy URLs on bindings in place (an invalid entry is cleared, so its account falls back to the form proxy).
//
// The three steps must not be reordered: write the proxy table first, then sync the proxy pool, and only then allow the accounts to be written. Once an account is bound to
// a URL that is "already in managedProxySet but not in proxyPoolSet", it is judged to have no usable
// egress and the whole batch becomes unschedulable; writing accounts before syncing the proxy pool opens exactly such a window. A sync failure is
// returned as an error, and the caller aborts the whole import: carrying on would only write a batch of unschedulable accounts.
func (h *Handler) registerImportedProxyBindings(ctx context.Context, bindings []importProxyBinding) (importProxyOutcome, error) {
	var outcome importProxyOutcome

	// Collect, validate and deduplicate. Invalid entries are skipped rather than failing the whole batch with a 400: one bad proxy should not ruin the import of the whole
	// account pool (this is deliberately different from the fail-fast of AddProxies, because an import needs more fault tolerance).
	resolved := make(map[string]string, len(bindings))
	seen := make(map[string]bool, len(bindings))
	ordered := make([]string, 0, len(bindings))
	for i := range bindings {
		raw := strings.TrimSpace(bindings[i].url)
		if raw == "" {
			continue
		}
		normalized, known := resolved[raw]
		if !known {
			var err error
			normalized, err = normalizeManagedProxyURL(raw)
			if err != nil {
				normalized = ""
				outcome.skipped++
				log.Printf("import proxy: skipped invalid proxy %s: %v", security.MaskURLCredentials(raw), err)
			}
			resolved[raw] = normalized
			if normalized != "" && !seen[normalized] {
				seen[normalized] = true
				ordered = append(ordered, normalized)
			}
		}
		bindings[i].url = normalized
	}
	if outcome.skipped > 0 {
		outcome.warnings = append(outcome.warnings,
			fmt.Sprintf("%d proxies have an invalid format and were skipped; their accounts use the form proxy instead", outcome.skipped))
	}
	if len(ordered) == 0 {
		return outcome, nil
	}

	if len(ordered) > maxImportedProxies {
		// When the limit is exceeded, none are registered. Registering half of them would leave the other half of the accounts bound to a URL that is not in the pool,
		// and that half-applied state is much harder to explain than "the whole thing falls back to the form proxy".
		for i := range bindings {
			bindings[i].url = ""
		}
		outcome.skipped += len(ordered)
		outcome.warnings = append(outcome.warnings,
			fmt.Sprintf("the file has %d proxies, over the %d per-import limit; all were skipped", len(ordered), maxImportedProxies))
		return outcome, nil
	}

	inserted, err := h.db.InsertProxies(ctx, ordered, importedProxyLabel())
	if err != nil {
		return outcome, fmt.Errorf("failed to write the proxy table: %w", err)
	}
	outcome.inserted = inserted

	if h.store != nil {
		if err := h.store.ReloadProxyPool(); err != nil {
			return outcome, fmt.Errorf("failed to refresh the proxy pool: %w", err)
		}
		if unusable := h.store.UnusableManagedProxies(ordered); len(unusable) > 0 {
			outcome.unusable = len(unusable)
			outcome.warnings = append(outcome.warnings,
				fmt.Sprintf("%d proxies are disabled or failed their test on this machine; accounts bound to them will not be scheduled for now", len(unusable)))
		}
	}
	if disabled := countDisabledAtSource(bindings); disabled > 0 {
		outcome.warnings = append(outcome.warnings,
			fmt.Sprintf("%d proxies are disabled at the source; they were imported as enabled. To keep them disabled, turn them off on the proxy management page", disabled))
	}

	log.Printf("import proxy: %d unique proxies in the file, %d added, %d skipped, %d unusable",
		len(ordered), outcome.inserted, outcome.skipped, outcome.unusable)
	return outcome, nil
}

// accountHasProxyBinding reports whether the account is currently bound to a proxy. It returns true when the query fails:
// it is better to keep the unknown original binding than to overwrite the operator's proxy because of one failed read.
//
// It must include accounts in the recycle bin: when an import hits an existing identity, that account is most likely sitting in the recycle bin (a deleted account re-imported
// goes through this merge/revive path), so a query that excludes deleted rows would never find it,
// so the existing binding could not be detected and an empty binding would never be filled with the file's proxy.
func (h *Handler) accountHasProxyBinding(ctx context.Context, accountID int64) bool {
	row, err := h.db.GetAccountByIDIncludingDeleted(ctx, accountID)
	if err != nil {
		log.Printf("import proxy: failed to read the existing proxy of account %d, treating it as bound: %v", accountID, err)
		return true
	}
	return row != nil && strings.TrimSpace(row.ProxyURL) != ""
}

// countDisabledAtSource counts the proxies marked disabled at the source. These proxies are always imported as enabled,
// because copying the disabled state would make the accounts bound to them fail closed at once, and the user would see "import succeeded
// but all the accounts are dead". Importing as enabled and saying so explicitly loses no information; the operator can disable them if needed.
func countDisabledAtSource(bindings []importProxyBinding) int {
	disabled := make(map[string]bool)
	for _, b := range bindings {
		proxyURL := strings.TrimSpace(b.url)
		if proxyURL == "" || b.enabled == nil || *b.enabled {
			continue
		}
		disabled[proxyURL] = true
	}
	return len(disabled)
}
