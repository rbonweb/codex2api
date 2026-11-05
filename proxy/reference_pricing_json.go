package proxy

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/codex2api/database"
)

const referencePricingBodyLimit = 32 << 20

// parseModelPricingPayload detects LiteLLM, models.dev or project override tables by their fields, and rejects mismatched types.
func parseModelPricingPayload(body []byte) (map[string]database.ModelPricingOverride, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse pricing JSON: %w", err)
	}
	for _, item := range raw {
		var fields map[string]json.RawMessage
		if json.Unmarshal(item, &fields) != nil {
			continue
		}
		if _, ok := fields["models"]; ok {
			var providers map[string]modelsDevProvider
			if err := json.Unmarshal(body, &providers); err != nil {
				return nil, fmt.Errorf("models.dev format mismatch: %w", err)
			}
			return convertModelsDevPricing(providers), nil
		}
		if _, ok := fields["litellm_provider"]; ok {
			return convertLiteLLMPricing(raw), nil
		}
	}
	return parseFlatModelPricing(raw)
}

// parseFlatModelPricing validates project and custom JSON, keeping valid partial overrides.
func parseFlatModelPricing(raw map[string]json.RawMessage) (map[string]database.ModelPricingOverride, error) {
	out := make(map[string]database.ModelPricingOverride)
	for model, item := range raw {
		var price database.ModelPricingOverride
		if err := json.Unmarshal(item, &price); err != nil {
			return nil, fmt.Errorf("%s pricing format mismatch: %w", model, err)
		}
		if err := validateSyncedPricing(model, price, false); err != nil {
			return nil, fmt.Errorf("%s pricing validation failed: %w", model, err)
		}
		out[strings.ToLower(strings.TrimSpace(model))] = price
	}
	return out, nil
}
