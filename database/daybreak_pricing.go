package database

import "strings"

// DaybreakProgram only affects rate selection and does not change the effective model in logs.
func daybreakBillingModel(model, program string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return model
	}
	switch program {
	case "daybreak_blue":
		return model + "-daybreak-blue"
	case "daybreak_red":
		return model + "-daybreak-red"
	default:
		return model
	}
}

// Daybreak-specific prices take precedence over the base model; a synced price must not override the admin's manually entered price.
func daybreakAliasPricing(baseModel, alias string) *ModelPricing {
	pricing := *GetModelPricing(baseModel)
	override, ok := lookupModelPricingOverride(alias)
	if !ok {
		return &pricing
	}
	if override.Source != ModelPricingSourceCustom {
		baseKey := PricingManagementModelKey(baseModel)
		canonicalKey := CanonicalBillingModelKey(baseModel)
		if ModelPricingSourceFor(baseKey) == ModelPricingSourceCustom ||
			ModelPricingSourceFor(canonicalKey) == ModelPricingSourceCustom {
			return &pricing
		}
	}
	override.applyNonZero(&pricing)
	return &pricing
}
