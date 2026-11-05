package proxy

import (
	"context"
	"fmt"
)

type pricingReferenceSource struct{ name, url string }

func pricingReferenceSources() []pricingReferenceSource {
	return []pricingReferenceSource{{"LiteLLM", LiteLLMPricingSyncURL}, {"models.dev", ModelsDevPricingSyncURL}, {"project default JSON", DefaultModelPricingSyncURL}}
}

// fetchReferences tries LiteLLM, models.dev, and the project JSON in order for each model; an existing higher-priority price is not overwritten.
func (sync *officialPricingSync) fetchReferences(ctx context.Context, proxyURL string) {
	for _, source := range pricingReferenceSources() {
		missing := sync.unpricedModels()
		if len(missing) == 0 || ctx.Err() != nil {
			break
		}
		parsed, err := fetchModelPricingJSON(ctx, source.url, proxyURL)
		if err != nil {
			sync.result.Warnings = append(sync.result.Warnings, fmt.Sprintf("%s reference price read failed: %v", source.name, err))
			continue
		}
		if imported := sync.importPricing(parsed); imported > 0 {
			sync.recordFallbacks(missing, source.url)
			sync.result.Sources = append(sync.result.Sources, source.url)
		}
	}
	sync.result.Fetched = len(sync.pricing)
}

// unpricedModels returns only the models among enabled providers that still have no valid price.
func (sync *officialPricingSync) unpricedModels() map[string]struct{} {
	missing := make(map[string]struct{})
	for model := range sync.allowed {
		if _, exists := sync.pricing[model]; !exists && officialPricingModelEnabled(model, sync.options) {
			missing[model] = struct{}{}
		}
	}
	return missing
}

// recordFallbacks records the source of each model that was actually filled with a fallback price, so official sync downgrades can be identified.
func (sync *officialPricingSync) recordFallbacks(missing map[string]struct{}, sourceURL string) {
	if sync.result.Fallbacks == nil {
		sync.result.Fallbacks = make(map[string]string)
	}
	for model := range missing {
		if _, exists := sync.pricing[model]; exists {
			sync.result.Fallbacks[model] = sourceURL
		}
	}
}

// referenceNotices generates a regular notice after custom prices are skipped, based on the reference price that was actually applied.
func (sync *officialPricingSync) referenceNotices() {
	counts := make(map[string]int)
	for _, sourceURL := range sync.result.Fallbacks {
		counts[sourceURL]++
	}
	for _, source := range pricingReferenceSources() {
		if count := counts[source.url]; count > 0 {
			sync.result.Notices = append(sync.result.Notices, fmt.Sprintf("%d models fell back to the %s reference price", count, source.name))
		}
	}
}
