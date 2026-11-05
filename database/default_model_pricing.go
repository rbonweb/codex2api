package database

// GetDefaultModelPricing returns a copy of the built-in billing rules and does not read admin or auto-sync overrides.
func GetDefaultModelPricing(model string) *ModelPricing {
	pricing := *baseModelPricing(normalizeBillingModelName(model), CanonicalBillingModelKey(model))
	return &pricing
}
