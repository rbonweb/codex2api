package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/database"
)

// DefaultModelPricingSyncURL is the project-maintained pricing JSON (raw GitHub). Deployers can change it on the settings page to
// their own mirror or private list. Format: { "<model>": {input,cached_input,output,...}, ... },
// see database.ModelPricingOverride.
const DefaultModelPricingSyncURL = "https://raw.githubusercontent.com/rbonweb/codex2api/main/pricing.json"

// ModelsDevPricingSyncURL is the models.dev public pricing API. Its format is provider→models→cost
// (USD / 1M tokens, including the 272K long-context tiers), and it is detected and converted automatically during sync.
const ModelsDevPricingSyncURL = "https://models.dev/api.json"

// modelPricingSyncURLForTest allows tests to replace the default URL. Production code must not assign it.
var modelPricingSyncURLForTest = ""

// ModelPricingSyncResult is the result projection of one pricing sync.
type ModelPricingSyncResult struct {
	SourceURL string `json:"source_url"`
	Fetched   int    `json:"fetched"` // number of models parsed from the URL
	Applied   int    `json:"applied"` // number of synced entries actually written
	Skipped   int    `json:"skipped"` // number of models skipped because an existing custom override applies
}

// SyncModelPricingFromURL fetches pricing from a JSON URL and writes it as synced overrides.
// Custom overrides take priority: sync does not overwrite existing custom entries (it only skips them, never clears them).
// On success it persists to system settings and refreshes the runtime override table.
func SyncModelPricingFromURL(ctx context.Context, db *database.DB, syncURL, proxyURL string) (*ModelPricingSyncResult, error) {
	if db == nil {
		return nil, fmt.Errorf("database unavailable, cannot sync model pricing")
	}
	// The field is authoritative: the syncURL passed in is the current value of the URL field on the settings page.
	// Non-empty: use it and store it as the source. Empty: use the built-in default and clear the stored source (fall back to default).
	fieldURL := strings.TrimSpace(syncURL)
	fetchURL := fieldURL
	if modelPricingSyncURLForTest != "" {
		fetchURL = modelPricingSyncURLForTest
	} else if fetchURL == "" {
		fetchURL = DefaultModelPricingSyncURL
	}
	result := &ModelPricingSyncResult{SourceURL: fetchURL}

	fetched, err := fetchModelPricingJSON(ctx, fetchURL, proxyURL)
	if err != nil {
		return result, err
	}
	result.Fetched = len(fetched)
	if len(fetched) == 0 {
		return result, fmt.Errorf("pricing source did not resolve any models")
	}

	_, err = db.MutateModelPricingSettings(ctx, &fieldURL, func(current map[string]database.ModelPricingOverride) error {
		for model, ov := range fetched {
			key := strings.ToLower(strings.TrimSpace(model))
			if key == "" || ov.IsEmpty() {
				continue
			}
			// An existing custom override takes priority, so skip it without touching it.
			if existing, ok := current[key]; ok && existing.Source == database.ModelPricingSourceCustom {
				result.Skipped++
				continue
			}
			ov.Source = database.ModelPricingSourceSynced
			current[key] = ov
			result.Applied++
		}
		return nil
	})
	if err != nil {
		return result, err
	}
	return result, nil
}

func fetchModelPricingJSON(ctx context.Context, syncURL, proxyURL string) (map[string]database.ModelPricingOverride, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, syncURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build pricing request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", subscriptionsBrowserUserAgent)

	// When the target is a GitHub domain (the default pricing JSON lives at raw.githubusercontent.com), apply the dedicated proxy;
	// the URL can be changed by the deployer to any address, so github_token is never attached here.
	client, closeClient := newVersionSyncClient(syncURL, GithubProxyOrDefault(syncURL, proxyURL), officialPricingTimeout)
	defer closeClient()
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pricing request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("pricing upstream status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, referencePricingBodyLimit+1))
	if err != nil {
		return nil, fmt.Errorf("read pricing response: %w", err)
	}
	if len(body) > referencePricingBodyLimit {
		return nil, fmt.Errorf("pricing JSON exceeds the size limit")
	}
	return parseModelPricingPayload(body)
}
