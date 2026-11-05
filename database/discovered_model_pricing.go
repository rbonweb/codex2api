package database

import (
	"regexp"
	"strings"
)

var (
	versionedGPTPricingID = regexp.MustCompile(`^gpt-\d+(?:\.\d+)?(?:-|$)`)
	gptPricingSnapshot    = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

// discoveredGPTPricingKey reserves a separate pricing key for GPT models that are not built in yet.
// Known models keep reusing the original pricing rules for their snapshots and reasoning-tier aliases.
func discoveredGPTPricingKey(model string) string {
	model = normalizeBillingModelName(model)
	model = strings.TrimSuffix(model, "-openai-compact")
	if cyberDefaultPricing(daybreakCanonicalModel(model)) != nil {
		return ""
	}
	if !versionedGPTPricingID.MatchString(model) {
		return ""
	}
	if index := strings.IndexByte(model, '('); index >= 0 {
		model = model[:index]
	}
	for _, effort := range []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"} {
		model = strings.TrimSuffix(model, "-"+effort)
	}
	for _, rule := range modelPricingRules {
		if model == rule.model {
			return ""
		}
		if suffix, ok := strings.CutPrefix(model, rule.model+"-"); ok && gptPricingSnapshot.MatchString(suffix) {
			return ""
		}
	}
	return model
}
