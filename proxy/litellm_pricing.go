package proxy

import (
	"encoding/json"
	"math"
	"math/big"
	"sort"
	"strings"

	"github.com/codex2api/database"
)

// LiteLLMPricingSyncURL is the structured price catalog maintained by LiteLLM.
const LiteLLMPricingSyncURL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"

const pricingTokensPerMillion = 1_000_000

type liteLLMPricingEntry map[string]json.RawMessage

// convertLiteLLMPricing imports only first-party providers and rejects same-named model prices from Azure and resellers.
func convertLiteLLMPricing(raw map[string]json.RawMessage) map[string]database.ModelPricingOverride {
	ids := make([]string, 0, len(raw))
	for id := range raw {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make(map[string]database.ModelPricingOverride)
	for _, id := range ids {
		var entry liteLLMPricingEntry
		if json.Unmarshal(raw[id], &entry) != nil {
			continue
		}
		model := liteLLMFirstPartyModel(id, entry)
		if model == "" {
			continue
		}
		price := liteLLMPriceToOverride(model, entry)
		if validateSyncedPricing(model, price, true) != nil {
			continue
		}
		if _, exists := out[model]; !exists {
			out[model] = price
		}
	}
	return out
}

// liteLLMFirstPartyModel validates identity by both provider and model namespace.
func liteLLMFirstPartyModel(id string, entry liteLLMPricingEntry) string {
	var provider string
	if json.Unmarshal(entry["litellm_provider"], &provider) != nil {
		return ""
	}
	prefix := map[string]string{"openai": ""}
	required, ok := prefix[provider]
	if !ok {
		return ""
	}
	id = strings.TrimPrefix(id, provider+"/")
	if strings.Contains(id, "/") || !strings.HasPrefix(id, required) {
		return ""
	}
	return database.PricingManagementModelKey(id)
}

// liteLLMPriceToOverride converts the per-token USD price to per-million tokens and keeps all context tiers.
func liteLLMPriceToOverride(model string, entry liteLLMPricingEntry) database.ModelPricingOverride {
	price := database.ModelPricingOverride{
		Input: entry.rate("input_cost_per_token"), CachedInput: entry.rate("cache_read_input_token_cost"), Output: entry.rate("output_cost_per_token"),
		InputPriority: entry.rate("input_cost_per_token_priority"), CachedInputPriority: entry.rate("cache_read_input_token_cost_priority"), OutputPriority: entry.rate("output_cost_per_token_priority"),
		ImageInput: entry.rate("input_cost_per_image_token"), CachedImageInput: entry.rate("cache_read_input_image_token_cost"),
		CacheWrite5m: entry.rate("cache_creation_input_token_cost"), CacheWrite1h: entry.rate("cache_creation_input_token_cost_above_1hr"),
	}
	if _, exists := entry["output_cost_per_token"]; !exists {
		price.Output = entry.rate("output_cost_per_image_token")
	}
	for _, threshold := range []struct {
		suffix string
		tokens int
	}{{"_above_272k_tokens", 272000}, {"_above_200k_tokens", 200000}, {"_above_100k_tokens", 100000}} {
		if _, ok := entry["input_cost_per_token"+threshold.suffix]; !ok {
			continue
		}
		price.InputLong = entry.rate("input_cost_per_token" + threshold.suffix)
		price.CachedInputLong = entry.rate("cache_read_input_token_cost" + threshold.suffix)
		price.OutputLong = entry.rate("output_cost_per_token" + threshold.suffix)
		price.InputLongPriority = entry.rate("input_cost_per_token" + threshold.suffix + "_priority")
		price.CachedInputLongPriority = entry.rate("cache_read_input_token_cost" + threshold.suffix + "_priority")
		price.OutputLongPriority = entry.rate("output_cost_per_token" + threshold.suffix + "_priority")
		price.LongContextThresholdTokens = threshold.tokens
		break
	}
	return price
}

// number distinguishes a missing field from a type error; the latter yields NaN so the unified validation rejects it.
func (entry liteLLMPricingEntry) number(key string) float64 {
	raw, ok := entry[key]
	if !ok || string(raw) == "null" {
		return 0
	}
	var value float64
	if json.Unmarshal(raw, &value) != nil {
		return math.NaN()
	}
	return value
}

// rate converts LiteLLM's per-token price to the project's unified USD / 1M tokens.
func (entry liteLLMPricingEntry) rate(key string) float64 {
	raw, exists := entry[key]
	if !exists || string(raw) == "null" {
		return 0
	}
	if !validPricingNumber(entry.number(key)) {
		return math.NaN()
	}
	price, ok := new(big.Rat).SetString(string(raw))
	if !ok {
		return math.NaN()
	}
	price.Mul(price, new(big.Rat).SetInt64(pricingTokensPerMillion))
	value, _ := price.Float64()
	return value
}
