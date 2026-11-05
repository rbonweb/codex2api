package proxy

import (
	"fmt"
	"math"

	"github.com/codex2api/database"
)

// validateSyncedPricing validates the numeric values, cache relationships, and required billing tiers of synced data.
func validateSyncedPricing(model string, price database.ModelPricingOverride, complete bool) error {
	groups := [][3]float64{
		{price.Input, price.CachedInput, price.Output},
		{price.InputPriority, price.CachedInputPriority, price.OutputPriority},
		{price.InputLong, price.CachedInputLong, price.OutputLong},
		{price.InputLongPriority, price.CachedInputLongPriority, price.OutputLongPriority},
		{price.ImageInput, price.CachedImageInput, 0},
	}
	for _, group := range groups {
		if err := validatePricingGroup(group); err != nil {
			return err
		}
	}
	if err := validatePricingExtras(price); err != nil {
		return err
	}
	if !complete {
		return nil
	}
	if target := database.DaybreakPricingTarget(model); target != "" {
		model = target
	}
	return validateRequiredPricing(model, price, groups[:len(groups)-1])
}

// validatePricingExtras validates prices and context thresholds that are not part of the token triple.
func validatePricingExtras(price database.ModelPricingOverride) error {
	for _, value := range []float64{price.MediaUnitCost, price.ImageUnitPrice, price.CacheWrite5m, price.CacheWrite1h} {
		if !validPricingNumber(value) {
			return fmt.Errorf("price must be a finite non-negative number")
		}
	}
	if price.LongContextThresholdTokens < 0 {
		return fmt.Errorf("long-context threshold cannot be negative")
	}
	return nil
}

// validateRequiredPricing validates the complete prices required for automatic sync; manual partial overrides do not go through this branch.
func validateRequiredPricing(model string, price database.ModelPricingOverride, groups [][3]float64) error {
	if database.MediaBillingUnit(model) != "" {
		if price.MediaUnitCost <= 0 {
			return fmt.Errorf("media unit cost is missing")
		}
		return nil
	}
	if price.Input <= 0 || price.Output <= 0 {
		return fmt.Errorf("input or output price is missing")
	}
	if err := validateCompletePricingGroups(groups); err != nil {
		return err
	}
	return validateLongContextPricing(model, price)
}

// validateCompletePricingGroups rejects incomplete billing tiers that have only a cache price or only an input price.
func validateCompletePricingGroups(groups [][3]float64) error {
	for _, group := range groups {
		if group[0] == 0 && group[1] == 0 && group[2] == 0 {
			continue
		}
		if group[0] <= 0 || group[2] <= 0 {
			return fmt.Errorf("input or output price is missing for the billing tier")
		}
	}
	return nil
}

// validatePricingGroup prevents output prices or cache-write prices from being written to the cache-read field.
func validatePricingGroup(group [3]float64) error {
	for _, value := range group {
		if !validPricingNumber(value) {
			return fmt.Errorf("price must be a finite non-negative number")
		}
	}
	if group[0] > 0 && group[1] > group[0] {
		return fmt.Errorf("cache read price is higher than the corresponding input price")
	}
	return nil
}

// validPricingNumber rejects negative prices, NaN, and infinity.
func validPricingNumber(value float64) bool {
	return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

// validateLongContextPricing rejects synced entries that lack the known long-context prices, to prevent old default tiers from being mixed in.
func validateLongContextPricing(model string, price database.ModelPricingOverride) error {
	base := database.GetDefaultModelPricing(model)
	knownModel := database.PricingManagementModelKey(model) == database.CanonicalBillingModelKey(model)
	requiresLong := (knownModel && base.LongInputPricePerMToken > 0) || price.LongContextThresholdTokens > 0
	if requiresLong && (price.InputLong <= 0 || price.OutputLong <= 0) {
		return fmt.Errorf("long-context input or output price is missing")
	}
	if price.InputLong > 0 && price.LongContextThresholdTokens <= 0 && base.LongInputPricePerMToken <= 0 {
		return fmt.Errorf("long-context price is missing its tier threshold")
	}
	return nil
}
