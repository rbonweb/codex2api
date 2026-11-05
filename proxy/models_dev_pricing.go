package proxy

import (
	"sort"

	"github.com/codex2api/database"
)

type modelsDevCostTier struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
	Tier       struct {
		Type string `json:"type"`
		Size int    `json:"size"`
	} `json:"tier"`
}

type modelsDevCost struct {
	modelsDevCostTier
	Tiers []modelsDevCostTier `json:"tiers"`
}

type modelsDevModel struct {
	Cost         *modelsDevCost `json:"cost"`
	Experimental struct {
		Modes map[string]struct {
			Cost *modelsDevCost `json:"cost"`
		} `json:"modes"`
	} `json:"experimental"`
}

type modelsDevProvider struct {
	Models map[string]modelsDevModel `json:"models"`
}

var modelsDevFirstPartyProviders = []string{"openai"}

// convertModelsDevPricing prefers canonical model IDs and merges only first-party provider data.
func convertModelsDevPricing(providers map[string]modelsDevProvider) map[string]database.ModelPricingOverride {
	models := mergeModelsDevProviders(providers, modelsDevFirstPartyProviders)
	ids := make([]string, 0, len(models))
	for id := range models {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		left := database.PricingManagementModelKey(ids[i]) == ids[i]
		right := database.PricingManagementModelKey(ids[j]) == ids[j]
		if left != right {
			return left
		}
		return ids[i] < ids[j]
	})
	out := make(map[string]database.ModelPricingOverride)
	for _, id := range ids {
		key := database.PricingManagementModelKey(id)
		if key == "" || models[id].Cost == nil {
			continue
		}
		price := modelsDevModelToOverride(models[id])
		if price.Input <= 0 || price.Output <= 0 || validateSyncedPricing(key, price, false) != nil {
			continue
		}
		if _, exists := out[key]; !exists {
			out[key] = price
		}
	}
	return out
}

// mergeModelsDevProviders merges by provider priority and forbids mixing same-named models from resellers.
func mergeModelsDevProviders(providers map[string]modelsDevProvider, names []string) map[string]modelsDevModel {
	merged := make(map[string]modelsDevModel)
	for _, name := range names {
		for id, model := range providers[name].Models {
			if _, exists := merged[id]; !exists {
				merged[id] = model
			}
		}
	}
	return merged
}

// modelsDevModelToOverride keeps the Fast price; missing Fast long-context tiers are not inferred on their own.
func modelsDevModelToOverride(model modelsDevModel) database.ModelPricingOverride {
	price := modelsDevCostToOverride(*model.Cost)
	if fast := model.Experimental.Modes["fast"].Cost; fast != nil {
		fastPrice := modelsDevCostToOverride(*fast)
		if price.InputLong > 0 && fastPrice.InputLong <= 0 {
			return price
		}
		price.InputPriority, price.CachedInputPriority, price.OutputPriority = fastPrice.Input, fastPrice.CachedInput, fastPrice.Output
		price.InputLongPriority, price.CachedInputLongPriority, price.OutputLongPriority = fastPrice.InputLong, fastPrice.CachedInputLong, fastPrice.OutputLong
	}
	return price
}

// modelsDevCostToOverride uses the context thresholds that the data source explicitly declares.
func modelsDevCostToOverride(cost modelsDevCost) database.ModelPricingOverride {
	price := database.ModelPricingOverride{Input: cost.Input, CachedInput: cost.CacheRead, Output: cost.Output, CacheWrite5m: cost.CacheWrite}
	for _, tier := range cost.Tiers {
		if tier.Tier.Type != "context" {
			continue
		}
		price.InputLong, price.CachedInputLong, price.OutputLong = tier.Input, tier.CacheRead, tier.Output
		price.LongContextThresholdTokens = tier.Tier.Size
		break
	}
	return price
}
