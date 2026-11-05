package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/codex2api/database"
)

const (
	OfficialOpenAIPricingURL = "https://developers.openai.com/api/docs/pricing"
	officialPricingTimeout   = 20 * time.Second
	officialPricingBodyLimit = 4 << 20
)

// OfficialPricingSyncOptions specifies the models and official sources for this sync round.
type OfficialPricingSyncOptions struct {
	Models        []string
	IncludeOpenAI bool
}

// OfficialPricingSyncResult contains the number of valid prices, the number of override updates, the models missing prices, and the reference sources.
type OfficialPricingSyncResult struct {
	Fetched   int               `json:"fetched"`
	Applied   int               `json:"applied"`
	Skipped   int               `json:"skipped"`
	Missing   []string          `json:"missing,omitempty"`
	Sources   []string          `json:"sources"`
	Notices   []string          `json:"notices,omitempty"`
	Warnings  []string          `json:"warnings,omitempty"`
	SyncedAt  time.Time         `json:"synced_at"`
	Fallbacks map[string]string `json:"fallbacks,omitempty"`
}

type officialPricingSource struct {
	name    string
	url     string
	enabled bool
	parse   func([]byte) (map[string]database.ModelPricingOverride, error)
}

type officialPricingSync struct {
	options OfficialPricingSyncOptions
	allowed map[string]struct{}
	pricing map[string]database.ModelPricingOverride
	result  *OfficialPricingSyncResult
}

// SyncOfficialModelPricing fetches prices outside the transaction, then atomically updates the overrides; when a price is missing it restores the default, and custom prices take precedence.
func SyncOfficialModelPricing(ctx context.Context, db *database.DB, proxyURL string, options OfficialPricingSyncOptions) (*OfficialPricingSyncResult, error) {
	if db == nil {
		return nil, fmt.Errorf("database unavailable, cannot sync official model prices")
	}
	sync := &officialPricingSync{
		options: options,
		allowed: make(map[string]struct{}),
		pricing: make(map[string]database.ModelPricingOverride),
		result:  &OfficialPricingSyncResult{SyncedAt: time.Now().UTC(), Sources: []string{}},
	}
	for _, model := range options.Models {
		if key := database.PricingManagementModelKey(model); key != "" {
			sync.allowed[key] = struct{}{}
		}
	}
	// Shares the browser transport with version sync: HTTPS uses uTLS, stays compatible with environment proxies, and releases connections explicitly.
	client, closeClient := newVersionSyncClient(OfficialOpenAIPricingURL, proxyURL, officialPricingTimeout)
	defer closeClient()
	sync.fetch(ctx, client)
	sync.fetchReferences(ctx, proxyURL)
	if err := ctx.Err(); err != nil {
		return sync.result, err
	}
	sync.findMissing()
	if len(sync.pricing) == 0 && len(sync.result.Missing) == 0 {
		return sync.result, fmt.Errorf("official page did not yield a price for the current model")
	}
	_, err := db.MutateModelPricingSettings(ctx, nil, sync.apply)
	if err == nil {
		sync.referenceNotices()
	}
	return sync.result, err
}

// fetch handles the failure of each official source independently, so that the other sources and the default-price fallback keep working.
func (sync *officialPricingSync) fetch(ctx context.Context, client *http.Client) {
	sources := []officialPricingSource{
		{"OpenAI", OfficialOpenAIPricingURL, sync.options.IncludeOpenAI, ParseOpenAIOfficialPricingHTML},
	}
	for _, source := range sources {
		if !source.enabled {
			continue
		}
		body, err := fetchOfficialPricingHTML(ctx, client, source.url)
		if err != nil {
			sync.result.Warnings = append(sync.result.Warnings, fmt.Sprintf("failed to read %s official prices: %v", source.name, err))
			continue
		}
		parsed, err := source.parse(body)
		if err != nil {
			sync.result.Warnings = append(sync.result.Warnings, fmt.Sprintf("failed to parse %s official prices: %v", source.name, err))
			continue
		}
		if source.url == OfficialOpenAIPricingURL {
			if err := openAIPriceData(parsed).supplementContext(ctx, client, body); err != nil {
				sync.result.Warnings = append(sync.result.Warnings, fmt.Sprintf("failed to read OpenAI long-context prices: %v", err))
			}
		}
		if sync.importPricing(parsed) > 0 {
			sync.result.Sources = append(sync.result.Sources, source.url)
		}
	}
	sync.result.Fetched = len(sync.pricing)
}

// importPricing only imports prices for models that appear in the current model list.
func (sync *officialPricingSync) importPricing(parsed map[string]database.ModelPricingOverride) int {
	imported := 0
	projectImageResolutionPricing(sync.allowed, parsed)
	projectDaybreakPricing(sync.allowed, parsed)
	for model, override := range parsed {
		if _, ok := sync.allowed[model]; len(sync.allowed) > 0 && !ok {
			continue
		}
		if _, exists := sync.pricing[model]; exists || !officialPricingModelEnabled(model, sync.options) {
			continue
		}
		if err := validateSyncedPricing(model, override, true); err != nil {
			sync.result.Warnings = append(sync.result.Warnings, fmt.Sprintf("%s price validation failed: %v", model, err))
			continue
		}
		sync.pricing[model] = override
		imported++
	}
	return imported
}

// findMissing only collects missing-price information for models from the sources enabled in this round.
func (sync *officialPricingSync) findMissing() {
	for model := range sync.allowed {
		if _, ok := sync.pricing[model]; ok || !officialPricingModelEnabled(model, sync.options) {
			continue
		}
		sync.result.Missing = append(sync.result.Missing, model)
	}
	sort.Strings(sync.result.Missing)
}

// officialPricingModelEnabled reports whether the model belongs to an official source enabled in this round.
func officialPricingModelEnabled(_ string, options OfficialPricingSyncOptions) bool {
	return options.IncludeOpenAI
}

// apply clears stale sync overrides for models with missing prices, so that the built-in rules and alias inheritance rules take effect again.
func (sync *officialPricingSync) apply(current map[string]database.ModelPricingOverride) error {
	for model, override := range sync.pricing {
		if current[model].Source == database.ModelPricingSourceCustom {
			delete(sync.result.Fallbacks, model)
			sync.result.Skipped++
			continue
		}
		override.Source = database.ModelPricingSourceSynced
		current[model] = override
		sync.result.Applied++
	}
	for _, model := range sync.result.Missing {
		if current[model].Source == database.ModelPricingSourceCustom {
			sync.result.Skipped++
			continue
		}
		delete(current, model)
	}
	return nil
}

// fetchOfficialPricingHTML only accepts HTML responses; when the format changes, the reference JSON takes over.
func fetchOfficialPricingHTML(ctx context.Context, client *http.Client, sourceURL string) ([]byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, officialPricingTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", subscriptionsBrowserUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("official pricing page returned %d", resp.StatusCode)
	}
	if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/html") {
		return nil, fmt.Errorf("official pricing page Content-Type is not HTML")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, officialPricingBodyLimit+1))
	if err != nil {
		return nil, err
	}
	if len(body) == 0 || len(body) > officialPricingBodyLimit {
		return nil, fmt.Errorf("official pricing page is empty or exceeds the size limit")
	}
	return body, nil
}
