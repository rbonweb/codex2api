package proxy

import "github.com/codex2api/database"

// projectDaybreakPricing matches each source by the underlying model, and does not require LiteLLM to list the local access-plan aliases.
// An exact alias price takes precedence; an unknown base model keeps falling back to the project default price and does not borrow another model's synced price.
func projectDaybreakPricing(allowed map[string]struct{}, prices map[string]database.ModelPricingOverride) {
	for model := range allowed {
		if _, exact := prices[model]; exact {
			continue
		}
		target := database.DaybreakPricingTarget(model)
		if target == "" || target == model {
			continue
		}
		if price, exists := prices[target]; exists {
			prices[model] = price
		}
	}
}
