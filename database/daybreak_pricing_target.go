package database

import (
	"regexp"
	"strings"
)

var cyberPricingID = regexp.MustCompile(`^(gpt-5\.[56]-cyber)(?:-\d{4}-\d{2}-\d{2})?(?:-(?:none|minimal|low|medium|high|xhigh|max|ultra))?(?:\([^()]+\))?$`)

// DaybreakPricingTarget returns the original base model of a locally derived alias, and does not guess an unknown model as another model.
// The default snapshot of latest is based on the official model page and is only a fallback when an alias cannot be resolved.
func DaybreakPricingTarget(model string) string {
	normalized := normalizeBillingModelName(model)
	for _, suffix := range []string{"-daybreak-blue", "-daybreak-red"} {
		if strings.HasSuffix(strings.ToLower(strings.TrimSpace(model)), suffix) {
			return PricingManagementModelKey(normalized)
		}
	}
	switch normalized {
	case "gpt-daybreak-blue-latest":
		return "gpt-5.6-sol"
	case "gpt-daybreak-red-latest":
		return "gpt-5.6-cyber"
	default:
		return ""
	}
}

// daybreakCanonicalModel distinguishes the published Cyber model and the latest default snapshot from Sol.
func daybreakCanonicalModel(model string) string {
	switch model {
	case "gpt-daybreak-blue-latest":
		return "gpt-5.6-sol"
	case "gpt-daybreak-red-latest":
		return "gpt-5.6-cyber"
	}
	if match := cyberPricingID.FindStringSubmatch(model); len(match) == 2 {
		return match[1]
	}
	return ""
}

// cyberDefaultPricing uses the official standalone price list; it does not fabricate long-context tiers that were not provided.
// https://developers.openai.com/api/docs/pricing (2026-10-22)
func cyberDefaultPricing(model string) *ModelPricing {
	if model != "gpt-5.6-cyber" && model != "gpt-5.5-cyber" {
		return nil
	}
	return &ModelPricing{InputPricePerMToken: 12.5, CacheReadPricePerMToken: 1.25, OutputPricePerMToken: 75}
}
