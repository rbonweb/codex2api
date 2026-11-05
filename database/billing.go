package database

import "strings"

const longContextThreshold = 272000

type ModelPricing struct {
	UserBillingMode                 string
	ImageUnitPrice                  float64
	ImageInputPricePerMToken        float64
	CacheReadImagePricePerMToken    float64
	InputPricePerMToken             float64
	InputPricePerMTokenPriority     float64
	OutputPricePerMToken            float64
	OutputPricePerMTokenPriority    float64
	CacheReadPricePerMToken         float64
	CacheReadPricePerMTokenPriority float64
	// CacheWrite* are prompt-cache creation prices (USD / 1M tokens).
	// They are surfaced for transparent pricing, while cost calculation continues
	// to use CacheReadPricePerMToken for cached input tokens.
	CacheWrite5mPricePerMToken float64
	CacheWrite1hPricePerMToken float64

	LongInputPricePerMToken             float64
	LongInputPricePerMTokenPriority     float64
	LongOutputPricePerMToken            float64
	LongOutputPricePerMTokenPriority    float64
	LongCacheReadPricePerMToken         float64
	LongCacheReadPricePerMTokenPriority float64

	// LongContextThresholdTokens is the input token threshold above which this model enters the long-context tier.
	// Leave empty to use the global longContextThreshold (272K for OpenAI); models with a different tier boundary must declare it in their own rule.
	LongContextThresholdTokens int

	// UltrafastMultiplier is the model's own Ultrafast tier multiplier (relative to the standard price).
	// When greater than 0, ultrafast requests are billed at the standard price times this multiplier, and do not go through Fast's 2× and
	// priority price; models with 0 keep ultrafast following the Fast policy. It does not take part in price overrides.
	UltrafastMultiplier float64
}

type modelPricingRule struct {
	model   string
	pricing ModelPricing
}

type CostBreakdown struct {
	ImageInputCost               float64 `json:"image_input_cost"`
	ImageCacheReadCost           float64 `json:"image_cache_read_cost"`
	ImageInputPricePerMToken     float64 `json:"image_input_price_per_mtoken"`
	CacheReadImagePricePerMToken float64 `json:"cached_image_input_price_per_mtoken"`
	InputCost                    float64 `json:"input_cost"`
	OutputCost                   float64 `json:"output_cost"`
	CacheReadCost                float64 `json:"cache_read_cost"`
	TotalCost                    float64 `json:"total_cost"`
	InputPricePerMToken          float64 `json:"input_price_per_mtoken"`
	OutputPricePerMToken         float64 `json:"output_price_per_mtoken"`
	CacheReadPricePerMToken      float64 `json:"cache_read_price_per_mtoken"`
	CacheWrite5mCost             float64 `json:"cache_write_5m_cost"`
	CacheWrite1hCost             float64 `json:"cache_write_1h_cost"`
	CacheWrite5mPricePerMToken   float64 `json:"cache_write_5m_price_per_mtoken"`
	CacheWrite1hPricePerMToken   float64 `json:"cache_write_1h_price_per_mtoken"`
	ServiceTierCostMultiplier    float64 `json:"service_tier_cost_multiplier"`
	LongContext                  bool    `json:"long_context"`
	LongContextThreshold         int     `json:"long_context_threshold"`
}

var (
	defaultModelPricing = &ModelPricing{InputPricePerMToken: 1.0, OutputPricePerMToken: 2.0}

	modelPricingRules = []modelPricingRule{
		// GPT-6 Sol/Luna use the official standard price and the long-context price above 272K; the fast tier keeps 2×.
		{model: "gpt-6-sol", pricing: ModelPricing{
			InputPricePerMToken:         2.0,
			OutputPricePerMToken:        10.0,
			CacheReadPricePerMToken:     0.2,
			LongInputPricePerMToken:     4.0,
			LongOutputPricePerMToken:    15.0,
			LongCacheReadPricePerMToken: 0.4,
		}},
		// gpt-6.1-sol is a standalone model: cache read $0.10, cache write $2.50,
		// above 272K the input/cache is 2× and output is 1.5×. It must not fall into the Astra fallback for unknown gpt-6 variants.
		{model: "gpt-6.1-sol", pricing: ModelPricing{
			InputPricePerMToken:         2.0,
			OutputPricePerMToken:        10.0,
			CacheReadPricePerMToken:     0.1,
			CacheWrite5mPricePerMToken:  2.5,
			CacheWrite1hPricePerMToken:  2.5,
			LongInputPricePerMToken:     4.0,
			LongOutputPricePerMToken:    15.0,
			LongCacheReadPricePerMToken: 0.2,
		}},
		// gpt-6.1-terra official standard price $1/$5, cache read $0.05, cache write $1.25,
		// above 272K it is $2/$7.50, cache $0.10; the fast tier keeps 2×.
		{model: "gpt-6.1-terra", pricing: ModelPricing{
			InputPricePerMToken:         1.0,
			OutputPricePerMToken:        5.0,
			CacheReadPricePerMToken:     0.05,
			CacheWrite5mPricePerMToken:  1.25,
			CacheWrite1hPricePerMToken:  1.25,
			LongInputPricePerMToken:     2.0,
			LongOutputPricePerMToken:    7.5,
			LongCacheReadPricePerMToken: 0.1,
		}},
		{model: "gpt-6-luna", pricing: ModelPricing{
			InputPricePerMToken:         0.1,
			OutputPricePerMToken:        0.5,
			CacheReadPricePerMToken:     0.01,
			LongInputPricePerMToken:     0.2,
			LongOutputPricePerMToken:    0.75,
			LongCacheReadPricePerMToken: 0.02,
		}},
		// gpt-6-astra: above 272K it is billed at the long-context price (input/cache 2×, output 1.5×),
		// i.e. $20/$75, cache $2. The fast (priority) 2× multiplier is provided as a fallback by serviceTierCostMultiplier;
		// the Ultrafast tier is billed at 6× the standard price of the tier it is in.
		{model: "gpt-6-astra", pricing: ModelPricing{
			InputPricePerMToken:         10.0,
			OutputPricePerMToken:        50.0,
			CacheReadPricePerMToken:     1.0,
			LongInputPricePerMToken:     20.0,
			LongOutputPricePerMToken:    75.0,
			LongCacheReadPricePerMToken: 2.0,
			UltrafastMultiplier:         6.0,
		}},
		{model: "gpt-5.5", pricing: ModelPricing{
			InputPricePerMToken:                 5.0,
			InputPricePerMTokenPriority:         12.5,
			OutputPricePerMToken:                30.0,
			OutputPricePerMTokenPriority:        75.0,
			CacheReadPricePerMToken:             0.5,
			CacheReadPricePerMTokenPriority:     1.25,
			LongInputPricePerMToken:             10.0,
			LongInputPricePerMTokenPriority:     25.0,
			LongOutputPricePerMToken:            45.0,
			LongOutputPricePerMTokenPriority:    112.5,
			LongCacheReadPricePerMToken:         1.0,
			LongCacheReadPricePerMTokenPriority: 2.5,
		}},
		{model: "gpt-5.5-pro", pricing: ModelPricing{
			InputPricePerMToken:              30.0,
			InputPricePerMTokenPriority:      75.0,
			OutputPricePerMToken:             180.0,
			OutputPricePerMTokenPriority:     450.0,
			LongInputPricePerMToken:          60.0,
			LongInputPricePerMTokenPriority:  150.0,
			LongOutputPricePerMToken:         270.0,
			LongOutputPricePerMTokenPriority: 675.0,
		}},
		// gpt-5.6-sol: standard is the same as gpt-5.5, but priority is 2× standard ($10/$60),
		// lower than gpt-5.5's 2.5×, so it does not reuse the gpt-5.5 entry. Leaving priority empty lets the fast tier fall back to 2×.
		{model: "gpt-5.6-sol", pricing: ModelPricing{
			InputPricePerMToken:         5.0,
			OutputPricePerMToken:        30.0,
			CacheReadPricePerMToken:     0.5,
			LongInputPricePerMToken:     10.0,
			LongOutputPricePerMToken:    45.0,
			LongCacheReadPricePerMToken: 1.0,
		}},
		// gpt-5.6-terra: official price cut of 20% on 2026-07-30 ($2/$12, previously $2.5/$15), priority 2×;
		// it has its own canonical key, so the pricing page can override it separately and it does not contaminate gpt-5.4.
		{model: "gpt-5.6-terra", pricing: ModelPricing{
			InputPricePerMToken:                 2.0,
			InputPricePerMTokenPriority:         4.0,
			OutputPricePerMToken:                12.0,
			OutputPricePerMTokenPriority:        24.0,
			CacheReadPricePerMToken:             0.2,
			CacheReadPricePerMTokenPriority:     0.4,
			LongInputPricePerMToken:             4.0,
			LongInputPricePerMTokenPriority:     8.0,
			LongOutputPricePerMToken:            18.0,
			LongOutputPricePerMTokenPriority:    36.0,
			LongCacheReadPricePerMToken:         0.4,
			LongCacheReadPricePerMTokenPriority: 0.8,
		}},
		// gpt-5.6-luna: official price cut of 80% on 2026-07-30 ($0.20/$1.20, previously $1/$6).
		// priority is 2× standard for all of them, derived automatically by the fast tier fallback.
		{model: "gpt-5.6-luna", pricing: ModelPricing{
			InputPricePerMToken:         0.2,
			OutputPricePerMToken:        1.2,
			CacheReadPricePerMToken:     0.02,
			LongInputPricePerMToken:     0.4,
			LongOutputPricePerMToken:    1.8,
			LongCacheReadPricePerMToken: 0.04,
		}},
		{model: "gpt-5.4-mini", pricing: ModelPricing{InputPricePerMToken: 0.75, OutputPricePerMToken: 4.5, CacheReadPricePerMToken: 0.075}},
		{model: "gpt-5.4-nano", pricing: ModelPricing{InputPricePerMToken: 0.2, OutputPricePerMToken: 1.25, CacheReadPricePerMToken: 0.02}},
		{model: "gpt-5.4", pricing: ModelPricing{
			InputPricePerMToken:                 2.5,
			InputPricePerMTokenPriority:         5.0,
			OutputPricePerMToken:                15.0,
			OutputPricePerMTokenPriority:        30.0,
			CacheReadPricePerMToken:             0.25,
			CacheReadPricePerMTokenPriority:     0.5,
			LongInputPricePerMToken:             5.0,
			LongInputPricePerMTokenPriority:     10.0,
			LongOutputPricePerMToken:            22.5,
			LongOutputPricePerMTokenPriority:    45.0,
			LongCacheReadPricePerMToken:         0.5,
			LongCacheReadPricePerMTokenPriority: 1.0,
		}},
		{model: "gpt-5.4-pro", pricing: ModelPricing{
			InputPricePerMToken:              30.0,
			InputPricePerMTokenPriority:      75.0,
			OutputPricePerMToken:             180.0,
			OutputPricePerMTokenPriority:     450.0,
			LongInputPricePerMToken:          60.0,
			LongInputPricePerMTokenPriority:  150.0,
			LongOutputPricePerMToken:         270.0,
			LongOutputPricePerMTokenPriority: 675.0,
		}},
		{model: "gpt-5.3-codex-spark", pricing: ModelPricing{
			InputPricePerMToken:             1.25,
			InputPricePerMTokenPriority:     2.5,
			OutputPricePerMToken:            10.0,
			OutputPricePerMTokenPriority:    20.0,
			CacheReadPricePerMToken:         0.125,
			CacheReadPricePerMTokenPriority: 0.25,
		}},
		{model: "gpt-5.3-codex", pricing: ModelPricing{
			InputPricePerMToken:             1.75,
			InputPricePerMTokenPriority:     3.5,
			OutputPricePerMToken:            14.0,
			OutputPricePerMTokenPriority:    28.0,
			CacheReadPricePerMToken:         0.175,
			CacheReadPricePerMTokenPriority: 0.35,
		}},
		{model: "gpt-5.2", pricing: ModelPricing{
			InputPricePerMToken:             1.75,
			InputPricePerMTokenPriority:     3.5,
			OutputPricePerMToken:            14.0,
			OutputPricePerMTokenPriority:    28.0,
			CacheReadPricePerMToken:         0.175,
			CacheReadPricePerMTokenPriority: 0.35,
		}},
		{model: "gpt-4o-mini", pricing: ModelPricing{InputPricePerMToken: 0.15, OutputPricePerMToken: 0.6}},
		{model: "gpt-4o", pricing: ModelPricing{InputPricePerMToken: 2.5, OutputPricePerMToken: 10.0}},
		{model: "gpt-4-turbo", pricing: ModelPricing{InputPricePerMToken: 10.0, OutputPricePerMToken: 30.0}},
		{model: "gpt-4", pricing: ModelPricing{InputPricePerMToken: 30.0, OutputPricePerMToken: 60.0}},
		{model: "gpt-3.5-turbo", pricing: ModelPricing{InputPricePerMToken: 0.5, OutputPricePerMToken: 1.5}},
	}
)

func GetModelPricing(model string) *ModelPricing {
	normalized := normalizeBillingModelName(model)
	if alias := daybreakPricingAliasKey(model); alias != "" && alias != normalized {
		return daybreakAliasPricing(normalized, alias)
	}
	canonical := CanonicalBillingModelKey(model)
	base := baseModelPricing(normalized, canonical)
	// New models keep the fallback base price, but overrides must be independent and must not inherit another model's manually entered price.
	if key := discoveredGPTPricingKey(normalized); key != "" {
		canonical = key
		if override, ok := lookupModelPricingOverride(key); ok && override.Input > 0 && override.Output > 0 {
			base = &ModelPricing{}
		}
	}

	// custom / synced overrides: start from the code defaults and merge the non-zero fields (partial override).
	// Copy the override table to a local copy before changing it; never modify the shared default pricing pointer.
	//
	// An explicit alias entry is more specific than the canonical model entry.
	// This matters for internal aliases such as codex-auto-review: it maps to
	// gpt-5.4 for fallback pricing, but its own price must not be shadowed by a
	// stale gpt-5.4 override. Two guardrails keep the documented order custom > synced > default:
	//   - synced alias entries must not override custom canonical entries, otherwise synced prices would silently replace
	//     admin-entered prices after an upgrade;
	//   - fields left unset on an alias entry fall back to the canonical effective price rather than the code default, so partial overrides
	//     do not quietly drop the remaining fields the admin set on the canonical entry.
	canonicalOv, hasCanonical := lookupModelPricingOverride(canonical)
	var aliasOv ModelPricingOverride
	hasAlias := false
	aliasKey := PricingManagementModelKey(model)
	if aliasKey != "" && aliasKey != canonical {
		if ov, ok := lookupModelPricingOverride(aliasKey); ok {
			if ov.Source == ModelPricingSourceCustom || !hasCanonical || canonicalOv.Source != ModelPricingSourceCustom {
				aliasOv, hasAlias = ov, true
			}
		}
	}
	if !hasCanonical && !hasAlias {
		return base
	}
	merged := *base
	if hasCanonical {
		canonicalOv.applyNonZero(&merged)
	}
	if hasAlias {
		aliasOv.applyNonZero(&merged)
	}
	return &merged
}

// baseModelPricing returns the built-in model price in code (without overrides). normalized is the normalized model name,
// canonical is the canonical name normalized by codex (used for rule table lookup).
func baseModelPricing(normalized, canonical string) *ModelPricing {
	if pricing := cyberDefaultPricing(canonical); pricing != nil {
		return pricing
	}
	if GPTImage25BillingModel(canonical) != "" {
		return &gptImage25Pricing
	}
	if pricing := modelRulePricing(canonical); pricing != nil {
		return pricing
	}
	return defaultModelPricing
}

func CalculateCost(inputTokens, outputTokens, cachedTokens int, model string, serviceTier string) float64 {
	return CalculateCostBreakdown(inputTokens, outputTokens, cachedTokens, model, serviceTier).TotalCost
}

// usageLogBillingServiceTier resolves the billing service tier of a usage event about to be written:
// an explicit BillingServiceTier takes precedence, then the upstream's actual tier, and finally the requested tier.
func usageLogBillingServiceTier(log *UsageLogInput) string {
	if log == nil {
		return ""
	}
	if tier := log.BillingServiceTier; tier != "" {
		return tier
	}
	if tier := log.ActualServiceTier; tier != "" {
		return tier
	}
	return log.ServiceTier
}

// UsageLogBilledCost returns upstream token cost. User quotas and scope budgets
// must use UsageLogUserBilledCost, which also supports per-image fees.
func UsageLogBilledCost(log *UsageLogInput) float64 {
	if log == nil {
		return 0
	}
	if log.billingSnapshot != nil {
		return log.billingSnapshot.accountCost
	}
	return UsageLogCostBreakdown(log).TotalCost
}

func CalculateCostBreakdown(inputTokens, outputTokens, cachedTokens int, model string, serviceTier string) CostBreakdown {
	return CalculateCostBreakdownWithCacheWrites(inputTokens, outputTokens, cachedTokens, 0, 0, model, serviceTier)
}

// CalculateCostBreakdownWithCacheWrites builds on CalculateCostBreakdown and includes
// prompt cache writes (5 minutes / 1 hour). inputTokens is the total input (uncached + cache hit + cache write),
// the write price defaults to 1.25× / 2× the input price.
func CalculateCostBreakdownWithCacheWrites(inputTokens, outputTokens, cachedTokens, cacheWrite5mTokens, cacheWrite1hTokens int, model string, serviceTier string) CostBreakdown {
	pricing := GetModelPricing(model)
	if GPTImage25BillingModel(model) != "" {
		return imageTokenCostBreakdown(inputTokens, outputTokens, cachedTokens, 0, 0, 0, pricing)
	}
	threshold := longContextThreshold
	if pricing.LongContextThresholdTokens > 0 {
		threshold = pricing.LongContextThresholdTokens
	}
	isLong := inputTokens >= threshold
	longContextApplied := false

	inputPrice := pricing.InputPricePerMToken
	outputPrice := pricing.OutputPricePerMToken
	cacheReadPrice := pricing.CacheReadPricePerMToken

	if isLong && pricing.LongInputPricePerMToken > 0 {
		longContextApplied = true
		inputPrice = pricing.LongInputPricePerMToken
		outputPrice = pricing.LongOutputPricePerMToken
		if pricing.LongCacheReadPricePerMToken > 0 {
			cacheReadPrice = pricing.LongCacheReadPricePerMToken
		}
	}

	tierMultiplier := serviceTierCostMultiplier(serviceTier)
	if normalizeServiceTier(serviceTier) == "ultrafast" && pricing.UltrafastMultiplier > 0 {
		tierMultiplier = pricing.UltrafastMultiplier
	} else if usePriorityPricing(serviceTier, pricing) {
		tierMultiplier = 1
		if isLong && pricing.LongInputPricePerMTokenPriority > 0 {
			inputPrice = pricing.LongInputPricePerMTokenPriority
		} else if pricing.InputPricePerMTokenPriority > 0 {
			inputPrice = pricing.InputPricePerMTokenPriority
		}
		if isLong && pricing.LongOutputPricePerMTokenPriority > 0 {
			outputPrice = pricing.LongOutputPricePerMTokenPriority
		} else if pricing.OutputPricePerMTokenPriority > 0 {
			outputPrice = pricing.OutputPricePerMTokenPriority
		}
		if isLong && pricing.LongCacheReadPricePerMTokenPriority > 0 {
			cacheReadPrice = pricing.LongCacheReadPricePerMTokenPriority
		} else if pricing.CacheReadPricePerMTokenPriority > 0 {
			cacheReadPrice = pricing.CacheReadPricePerMTokenPriority
		}
	}

	if cachedTokens < 0 {
		cachedTokens = 0
	}
	if cachedTokens > inputTokens {
		cachedTokens = inputTokens
	}
	if cacheWrite5mTokens < 0 {
		cacheWrite5mTokens = 0
	}
	if cacheWrite1hTokens < 0 {
		cacheWrite1hTokens = 0
	}
	cacheWrite5mPrice := pricing.CacheWrite5mPricePerMToken
	if cacheWrite5mPrice <= 0 {
		cacheWrite5mPrice = inputPrice * 1.25
	}
	cacheWrite1hPrice := pricing.CacheWrite1hPricePerMToken
	if cacheWrite1hPrice <= 0 {
		cacheWrite1hPrice = inputPrice * 2
	}

	uncachedInputTokens := inputTokens
	if cacheReadPrice > 0 {
		uncachedInputTokens = inputTokens - cachedTokens
	}
	uncachedInputTokens -= cacheWrite5mTokens + cacheWrite1hTokens
	if uncachedInputTokens < 0 {
		uncachedInputTokens = 0
	}

	inputCost := float64(uncachedInputTokens) / 1000000.0 * inputPrice
	cacheReadCost := float64(cachedTokens) / 1000000.0 * cacheReadPrice
	cacheWrite5mCost := float64(cacheWrite5mTokens) / 1000000.0 * cacheWrite5mPrice
	cacheWrite1hCost := float64(cacheWrite1hTokens) / 1000000.0 * cacheWrite1hPrice
	outputCost := float64(outputTokens) / 1000000.0 * outputPrice

	return CostBreakdown{
		InputCost:                  inputCost * tierMultiplier,
		OutputCost:                 outputCost * tierMultiplier,
		CacheReadCost:              cacheReadCost * tierMultiplier,
		CacheWrite5mCost:           cacheWrite5mCost * tierMultiplier,
		CacheWrite1hCost:           cacheWrite1hCost * tierMultiplier,
		TotalCost:                  (inputCost + cacheReadCost + cacheWrite5mCost + cacheWrite1hCost + outputCost) * tierMultiplier,
		InputPricePerMToken:        inputPrice * tierMultiplier,
		OutputPricePerMToken:       outputPrice * tierMultiplier,
		CacheReadPricePerMToken:    cacheReadPrice * tierMultiplier,
		CacheWrite5mPricePerMToken: cacheWrite5mPrice * tierMultiplier,
		CacheWrite1hPricePerMToken: cacheWrite1hPrice * tierMultiplier,
		ServiceTierCostMultiplier:  tierMultiplier,
		LongContext:                longContextApplied,
		LongContextThreshold:       threshold,
	}
}

func normalizeBillingModelName(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	model = strings.TrimLeft(model, "/")
	model = strings.TrimPrefix(model, "models/")
	model = strings.TrimPrefix(model, "publishers/google/models/")
	if idx := strings.LastIndex(model, "/publishers/google/models/"); idx != -1 {
		model = model[idx+len("/publishers/google/models/"):]
	}
	if idx := strings.LastIndex(model, "/models/"); idx != -1 {
		model = model[idx+len("/models/"):]
	} else if idx := strings.LastIndex(model, "/"); idx != -1 {
		model = model[idx+1:]
	}
	// Daybreak's canonical billing model is still the base model; dedicated price overrides are handled by alias keys.
	model = strings.TrimSuffix(strings.TrimSuffix(model, "-daybreak-blue"), "-daybreak-red")
	return strings.TrimLeft(model, "/")
}

func normalizeCodexBillingModel(model string) (string, bool) {
	compact := strings.NewReplacer(" ", "-", "_", "-").Replace(strings.ToLower(model))
	if target := daybreakCanonicalModel(compact); target != "" {
		return target, true
	}
	switch {
	case strings.HasPrefix(compact, "gpt-6-sol") || strings.HasPrefix(compact, "gpt6-sol"):
		return "gpt-6-sol", true
	case strings.HasPrefix(compact, "gpt-6.1-sol") || strings.HasPrefix(compact, "gpt6.1-sol"):
		return "gpt-6.1-sol", true
	case strings.HasPrefix(compact, "gpt-6.1-terra") || strings.HasPrefix(compact, "gpt6.1-terra"):
		return "gpt-6.1-terra", true
	case strings.HasPrefix(compact, "gpt-6-luna") || strings.HasPrefix(compact, "gpt6-luna"):
		return "gpt-6-luna", true
	// Unknown gpt-6 variants fall back to astra, so they do not drop into the $1/$2 default price and badly underestimate.
	// Only the gpt-6- / gpt-6. / bare gpt-6 prefixes match; gpt-5.6 does not contain "gpt-6", so it cannot match by mistake.
	case strings.HasPrefix(compact, "gpt-6-") || strings.HasPrefix(compact, "gpt-6.") || compact == "gpt-6" ||
		strings.HasPrefix(compact, "gpt6-") || strings.HasPrefix(compact, "gpt6.") || compact == "gpt6":
		return "gpt-6-astra", true
	case strings.Contains(compact, "gpt-5.5-pro") || strings.Contains(compact, "gpt5-5-pro") || strings.Contains(compact, "gpt5.5-pro"):
		return "gpt-5.5-pro", true
	case strings.Contains(compact, "gpt-5.5") || strings.Contains(compact, "gpt5-5") || strings.Contains(compact, "gpt5.5"):
		return "gpt-5.5", true
	// The three GPT-5.6 variants have different official prices (developers.openai.com/api/docs/pricing):
	//   sol   $5/$30 (standard, priority 2× = $10/$60): same as gpt-5.5 standard, but a lower priority price
	//   terra $2/$12 (cut 20% on 2026-07-30, previously $2.5/$15): its own canonical key, configurable separately on the pricing page
	//   luna  $0.20/$1.20 (cut 80% on 2026-07-30, previously $1/$6)
	// priority is 2× standard for all of them, derived automatically by the serviceTierCostMultiplier fallback, no explicit configuration needed.
	case strings.Contains(compact, "gpt-5.6-sol") || strings.Contains(compact, "gpt5-6-sol") || strings.Contains(compact, "gpt5.6-sol"):
		return "gpt-5.6-sol", true
	case strings.Contains(compact, "gpt-5.6-terra") || strings.Contains(compact, "gpt5-6-terra") || strings.Contains(compact, "gpt5.6-terra"):
		return "gpt-5.6-terra", true
	case strings.Contains(compact, "gpt-5.6-luna") || strings.Contains(compact, "gpt5-6-luna") || strings.Contains(compact, "gpt5.6-luna"):
		return "gpt-5.6-luna", true
	case strings.Contains(compact, "gpt-5.6") || strings.Contains(compact, "gpt5-6") || strings.Contains(compact, "gpt5.6"):
		// Unknown gpt-5.6 variants fall back to sol; Cyber is already handled above as a separate price list.
		return "gpt-5.6-sol", true
	case strings.HasPrefix(compact, "gpt-daybreak-"):
		// Unknown code-name aliases only keep the project's existing Sol fallback and do not generate synced prices from it.
		return "gpt-5.6-sol", true
	case strings.Contains(compact, "gpt-5.4-mini") || strings.Contains(compact, "gpt5-4-mini") || strings.Contains(compact, "gpt5.4-mini"):
		return "gpt-5.4-mini", true
	case strings.Contains(compact, "gpt-5.4-nano") || strings.Contains(compact, "gpt5-4-nano") || strings.Contains(compact, "gpt5.4-nano"):
		return "gpt-5.4-nano", true
	case strings.Contains(compact, "gpt-5.4-pro") || strings.Contains(compact, "gpt5-4-pro") || strings.Contains(compact, "gpt5.4-pro"):
		return "gpt-5.4-pro", true
	case strings.Contains(compact, "gpt-5.4") || strings.Contains(compact, "gpt5-4") || strings.Contains(compact, "gpt5.4"):
		return "gpt-5.4", true
	case strings.Contains(compact, "gpt-5.2") || strings.Contains(compact, "gpt5-2") || strings.Contains(compact, "gpt5.2"):
		return "gpt-5.2", true
	case strings.Contains(compact, "gpt-5.3-codex-spark") || strings.Contains(compact, "gpt5-3-codex-spark") || strings.Contains(compact, "gpt5.3-codex-spark"):
		return "gpt-5.3-codex-spark", true
	case strings.Contains(compact, "gpt-5.3-codex") || strings.Contains(compact, "gpt5-3-codex") || strings.Contains(compact, "gpt5.3-codex"):
		return "gpt-5.3-codex", true
	case strings.Contains(compact, "gpt-5.3") || strings.Contains(compact, "gpt5-3") || strings.Contains(compact, "gpt5.3"):
		return "gpt-5.3-codex", true
	case strings.Contains(compact, "codex-auto-review"):
		// Codex internal auto-review model. ChatGPT backend API only
		// (chatgpt.com/backend-api/codex). Not available via public API.
		// Official catalog: Plus/Pro/Team/Business only, excludes free.
		// Specs match gpt-5.4 (272K context, 4 thinking levels).
		return "gpt-5.4", true
	case strings.Contains(compact, "codex"):
		return "gpt-5.3-codex", true
	case strings.Contains(compact, "gpt-5") || strings.Contains(compact, "gpt5"):
		return "gpt-5.4", true
	default:
		return "", false
	}
}

func modelRulePricing(model string) *ModelPricing {
	bestIdx := -1
	bestLen := -1
	for i := range modelPricingRules {
		rule := modelPricingRules[i]
		if modelMatchesRule(model, rule.model) && len(rule.model) > bestLen {
			bestIdx = i
			bestLen = len(rule.model)
		}
	}
	if bestIdx == -1 {
		return nil
	}
	return &modelPricingRules[bestIdx].pricing
}

func modelMatchesRule(model string, rule string) bool {
	if model == rule {
		return true
	}
	if !strings.HasPrefix(model, rule) {
		return false
	}
	if len(model) == len(rule) {
		return true
	}
	switch model[len(rule)] {
	case '-', '.', ':':
		return true
	default:
		return false
	}
}

func usePriorityPricing(serviceTier string, pricing *ModelPricing) bool {
	tier := normalizeServiceTier(serviceTier)
	if tier != "priority" && tier != "fast" && tier != "ultrafast" {
		return false
	}
	return pricing.InputPricePerMTokenPriority > 0 ||
		pricing.OutputPricePerMTokenPriority > 0 ||
		pricing.CacheReadPricePerMTokenPriority > 0
}

func serviceTierCostMultiplier(serviceTier string) float64 {
	switch normalizeServiceTier(serviceTier) {
	// Ultrafast follows this gateway's Fast pricing policy, including custom
	// priority prices, unless the model declares its own UltrafastMultiplier.
	// This is a billing default, not an official tariff claim.
	case "priority", "fast", "ultrafast":
		return 2.0
	case "flex":
		return 0.5
	default:
		return 1.0
	}
}

func normalizeServiceTier(serviceTier string) string {
	return strings.ToLower(strings.TrimSpace(serviceTier))
}

// lowercase aliases for internal callers
func calculateCost(inputTokens, outputTokens, cachedTokens int, model string, serviceTier string) float64 {
	return CalculateCost(inputTokens, outputTokens, cachedTokens, model, serviceTier)
}

func calculateCostBreakdown(inputTokens, outputTokens, cachedTokens int, model string, serviceTier string) CostBreakdown {
	return CalculateCostBreakdown(inputTokens, outputTokens, cachedTokens, model, serviceTier)
}
