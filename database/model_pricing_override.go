package database

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
)

// Source of a model pricing override. custom is entered by hand by an admin (highest priority), synced is written by "Sync from JSON URL".
const (
	ModelPricingSourceCustom = "custom"
	ModelPricingSourceSynced = "synced"
)

// ModelPricingOverride is the override for a single model (shared by the settings storage and the sync URL payload).
// Token price unit: USD / 1M tokens. When a token field is 0, it falls back to the code default price.
// ImageUnitPrice is the user unit price (USD per billable unit in per-image mode), which requires an explicit positive unit price in that mode;
// Priority: custom > synced > code default.
type ModelPricingOverride struct {
	UserBillingMode  string  `json:"user_billing_mode,omitempty"`
	ImageUnitPrice   float64 `json:"image_unit_price,omitempty"` // USD per billed unit (image) in unit billing mode; upstream cost stays independent.
	ImageInput       float64 `json:"image_input,omitempty"`
	CachedImageInput float64 `json:"cached_image_input,omitempty"`
	Source           string  `json:"source,omitempty"`

	// Standard tier (short context)
	Input       float64 `json:"input,omitempty"`
	CachedInput float64 `json:"cached_input,omitempty"`
	// Prompt-cache creation prices are informational today; actual
	// billing still uses CachedInput (cache read).
	CacheWrite5m float64 `json:"cache_write_5m,omitempty"`
	CacheWrite1h float64 `json:"cache_write_1h,omitempty"`
	Output       float64 `json:"output,omitempty"`

	// priority (fast) tier
	InputPriority       float64 `json:"input_priority,omitempty"`
	CachedInputPriority float64 `json:"cached_input_priority,omitempty"`
	OutputPriority      float64 `json:"output_priority,omitempty"`

	// Long-context tier (input above the model's tier threshold: OpenAI 272K)
	InputLong       float64 `json:"input_long,omitempty"`
	CachedInputLong float64 `json:"cached_input_long,omitempty"`
	OutputLong      float64 `json:"output_long,omitempty"`

	// Long-context + priority/fast tier. OpenAI's official price list publishes this group of prices separately,
	// so it can no longer be derived from the short-context priority or long-context standard multipliers.
	InputLongPriority       float64 `json:"input_long_priority,omitempty"`
	CachedInputLongPriority float64 `json:"cached_input_long_priority,omitempty"`
	OutputLongPriority      float64 `json:"output_long_priority,omitempty"`

	// The official long-context threshold. OpenAI's is currently 272K; save this value alongside it,
	// so a new model with only price overrides does not wrongly inherit the global default threshold.
	LongContextThresholdTokens int `json:"long_context_threshold_tokens,omitempty"`
}

// NormalizeModelPricingOverride keeps the long-context tiers in the model override.
// Long-context prices are decided by each model's own rules; they must not be zeroed globally for Astra.
func NormalizeModelPricingOverride(model string, o ModelPricingOverride) ModelPricingOverride {
	return o
}

// IsEmpty reports whether the override contains no prices (all zero).
func (o ModelPricingOverride) IsEmpty() bool {
	return o.UserBillingMode == "" && o.ImageUnitPrice == 0 && o.ImageInput == 0 && o.CachedImageInput == 0 && o.Input == 0 && o.CachedInput == 0 && o.CacheWrite5m == 0 && o.CacheWrite1h == 0 && o.Output == 0 &&
		o.InputPriority == 0 && o.CachedInputPriority == 0 && o.OutputPriority == 0 &&
		o.InputLong == 0 && o.CachedInputLong == 0 && o.OutputLong == 0 &&
		o.InputLongPriority == 0 && o.CachedInputLongPriority == 0 && o.OutputLongPriority == 0 &&
		o.LongContextThresholdTokens == 0
}

// applyNonZero writes the non-zero price fields of the override into p (in place). Zero fields keep p's original value (the code default).
func (o ModelPricingOverride) applyNonZero(p *ModelPricing) {
	if o.UserBillingMode != "" {
		p.UserBillingMode = o.UserBillingMode
	}
	if o.ImageUnitPrice > 0 {
		p.ImageUnitPrice = o.ImageUnitPrice
	}
	if o.ImageInput > 0 {
		p.ImageInputPricePerMToken = o.ImageInput
	}
	if o.CachedImageInput > 0 {
		p.CacheReadImagePricePerMToken = o.CachedImageInput
	}
	if o.Input > 0 {
		p.InputPricePerMToken = o.Input
	}
	if o.CachedInput > 0 {
		p.CacheReadPricePerMToken = o.CachedInput
	}
	if o.CacheWrite5m > 0 {
		p.CacheWrite5mPricePerMToken = o.CacheWrite5m
	}
	if o.CacheWrite1h > 0 {
		p.CacheWrite1hPricePerMToken = o.CacheWrite1h
	}
	if o.Output > 0 {
		p.OutputPricePerMToken = o.Output
	}
	if o.InputPriority > 0 {
		p.InputPricePerMTokenPriority = o.InputPriority
	}
	if o.CachedInputPriority > 0 {
		p.CacheReadPricePerMTokenPriority = o.CachedInputPriority
	}
	if o.OutputPriority > 0 {
		p.OutputPricePerMTokenPriority = o.OutputPriority
	}
	if o.InputLong > 0 {
		p.LongInputPricePerMToken = o.InputLong
	}
	if o.CachedInputLong > 0 {
		p.LongCacheReadPricePerMToken = o.CachedInputLong
	}
	if o.OutputLong > 0 {
		p.LongOutputPricePerMToken = o.OutputLong
	}
	if o.InputLongPriority > 0 {
		p.LongInputPricePerMTokenPriority = o.InputLongPriority
	}
	if o.CachedInputLongPriority > 0 {
		p.LongCacheReadPricePerMTokenPriority = o.CachedInputLongPriority
	}
	if o.OutputLongPriority > 0 {
		p.LongOutputPricePerMTokenPriority = o.OutputLongPriority
	}
	if o.LongContextThresholdTokens > 0 {
		p.LongContextThresholdTokens = o.LongContextThresholdTokens
	}
}

// ModelPricingOverrideFromPricing projects a complete ModelPricing into the override JSON shape,
// for the admin view of the "currently effective price" and the initial values for editing.
func ModelPricingOverrideFromPricing(p *ModelPricing, source string) ModelPricingOverride {
	if p == nil {
		return ModelPricingOverride{Source: source}
	}
	return ModelPricingOverride{
		UserBillingMode:            p.UserBillingMode,
		ImageUnitPrice:             p.ImageUnitPrice,
		ImageInput:                 p.ImageInputPricePerMToken,
		CachedImageInput:           p.CacheReadImagePricePerMToken,
		Source:                     source,
		Input:                      p.InputPricePerMToken,
		CachedInput:                p.CacheReadPricePerMToken,
		CacheWrite5m:               p.CacheWrite5mPricePerMToken,
		CacheWrite1h:               p.CacheWrite1hPricePerMToken,
		Output:                     p.OutputPricePerMToken,
		InputPriority:              p.InputPricePerMTokenPriority,
		CachedInputPriority:        p.CacheReadPricePerMTokenPriority,
		OutputPriority:             p.OutputPricePerMTokenPriority,
		InputLong:                  p.LongInputPricePerMToken,
		CachedInputLong:            p.LongCacheReadPricePerMToken,
		OutputLong:                 p.LongOutputPricePerMToken,
		InputLongPriority:          p.LongInputPricePerMTokenPriority,
		CachedInputLongPriority:    p.LongCacheReadPricePerMTokenPriority,
		OutputLongPriority:         p.LongOutputPricePerMTokenPriority,
		LongContextThresholdTokens: p.LongContextThresholdTokens,
	}
}

// pricingOverrides stores map[string]ModelPricingOverride (the key is the normalized model name, lower case).
var pricingOverrides atomic.Value

// modelPricingMutationSlot serializes the very short read/merge/write phase of
// manual, JSON-reference, and official pricing updates. Network fetching must
// always finish before entering this slot.
var modelPricingMutationSlot = func() chan struct{} {
	ch := make(chan struct{}, 1)
	ch <- struct{}{}
	return ch
}()

// MutateModelPricingSettings applies one in-process atomic read/merge/write.
// syncURL=nil preserves the current manual JSON source; a non-nil value replaces
// it (including an empty string). The callback must not perform network I/O.
func (db *DB) MutateModelPricingSettings(ctx context.Context, syncURL *string, mutate func(map[string]ModelPricingOverride) error) (map[string]ModelPricingOverride, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-modelPricingMutationSlot:
	}
	defer func() { modelPricingMutationSlot <- struct{}{} }()

	settings, err := db.GetSystemSettings(ctx)
	if err != nil {
		return nil, err
	}
	if settings == nil {
		settings = &SystemSettings{}
	}
	overrides, err := ParseModelPricingOverridesJSON(settings.ModelPricingOverrides)
	if err != nil {
		return nil, fmt.Errorf("failed to parse model pricing overrides, write aborted to avoid clearing existing prices: %w", err)
	}
	if mutate != nil {
		if err := mutate(overrides); err != nil {
			return nil, err
		}
	}
	for model, override := range overrides {
		normalized := NormalizeModelPricingOverride(model, override)
		if normalized.IsEmpty() {
			delete(overrides, model)
		} else {
			overrides[model] = normalized
		}
	}
	blob, err := MarshalModelPricingOverridesJSON(overrides)
	if err != nil {
		return nil, err
	}
	effectiveSyncURL := settings.ModelPricingSyncURL
	if syncURL != nil {
		effectiveSyncURL = *syncURL
	}
	if err := db.UpdateModelPricingSettings(ctx, blob, effectiveSyncURL); err != nil {
		return nil, err
	}
	SetModelPricingOverrides(overrides)
	return overrides, nil
}

// SetModelPricingOverrides refreshes the runtime pricing override table (keys are normalized to lower case with whitespace removed).
func SetModelPricingOverrides(m map[string]ModelPricingOverride) {
	norm := make(map[string]ModelPricingOverride, len(m))
	for k, v := range m {
		key := strings.ToLower(strings.TrimSpace(k))
		if err := ValidateModelUserBilling(key, v); err != nil {
			continue
		}
		v = NormalizeModelPricingOverride(key, v)
		if key == "" || v.IsEmpty() {
			continue
		}
		norm[key] = v
	}
	pricingOverrides.Store(norm)
}

func currentModelPricingOverrides() map[string]ModelPricingOverride {
	if v, ok := pricingOverrides.Load().(map[string]ModelPricingOverride); ok {
		return v
	}
	return nil
}

// lookupModelPricingOverride looks up the override by normalized model name.
func lookupModelPricingOverride(canonical string) (ModelPricingOverride, bool) {
	m := currentModelPricingOverrides()
	if m == nil {
		return ModelPricingOverride{}, false
	}
	ov, ok := m[strings.ToLower(strings.TrimSpace(canonical))]
	return ov, ok
}

// CanonicalBillingModelKey returns the canonical key (lower case) that a model uses for pricing lookup and overrides.
func CanonicalBillingModelKey(model string) string {
	normalized := normalizeBillingModelName(model)
	if imageModel := GPTImage25BillingModel(normalized); imageModel != "" {
		return imageModel
	}
	if codexModel, ok := normalizeCodexBillingModel(normalized); ok {
		return strings.ToLower(codexModel)
	}
	return strings.ToLower(normalized)
}

// PricingManagementModelKey returns the key exposed by pricing management.
// Most model variants share their canonical model's price, but selected
// internal aliases have an independent override and therefore need their own
// editable row instead of being deduplicated into the canonical model.
func PricingManagementModelKey(model string) string {
	if key := daybreakPricingAliasKey(model); key != "" {
		return key
	}
	if key := discoveredGPTPricingKey(model); key != "" {
		return key
	}
	normalized := normalizeBillingModelName(model)
	compact := strings.NewReplacer(" ", "-", "_", "-").Replace(normalized)
	if compact == "codex-auto-review" {
		return compact
	}
	return CanonicalBillingModelKey(normalized)
}

func daybreakPricingAliasKey(model string) string {
	normalized := normalizeBillingModelName(model)
	if normalized == "" {
		return ""
	}
	lower := strings.ToLower(strings.TrimSpace(model))
	for _, suffix := range []string{"-daybreak-blue", "-daybreak-red"} {
		if strings.HasSuffix(lower, suffix) {
			return normalized + suffix
		}
	}
	if strings.HasPrefix(normalized, "gpt-daybreak-") {
		return normalized
	}
	return ""
}

// PricingAliasTarget reports the canonical fallback for an independently
// managed alias. An empty string means the model is already canonical.
func PricingAliasTarget(model string) string {
	if discoveredGPTPricingKey(model) != "" && daybreakPricingAliasKey(model) == "" {
		return ""
	}
	managed := PricingManagementModelKey(model)
	canonical := CanonicalBillingModelKey(model)
	if managed != canonical {
		return canonical
	}
	return ""
}

// ModelPricingSourceFor returns the current pricing source for a canonical key: custom / synced / default.
func ModelPricingSourceFor(canonical string) string {
	if ov, ok := lookupModelPricingOverride(canonical); ok {
		if ov.Source == ModelPricingSourceCustom {
			return ModelPricingSourceCustom
		}
		return ModelPricingSourceSynced
	}
	if target := PricingAliasTarget(canonical); target != "" {
		if ov, ok := lookupModelPricingOverride(target); ok {
			if ov.Source == ModelPricingSourceCustom {
				return ModelPricingSourceCustom
			}
			return ModelPricingSourceSynced
		}
	}
	return "default"
}

// ParseModelPricingOverridesJSON parses the stored JSON blob (model → override).
// An empty string returns an empty map and a nil error.
func ParseModelPricingOverridesJSON(s string) (map[string]ModelPricingOverride, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "{}" {
		return map[string]ModelPricingOverride{}, nil
	}
	var raw map[string]ModelPricingOverride
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil, err
	}
	out := make(map[string]ModelPricingOverride, len(raw))
	for k, v := range raw {
		key := strings.ToLower(strings.TrimSpace(k))
		if isRemovedMediaBillingMode(v.UserBillingMode) {
			// Entries saved for the removed video billing modes are dropped so they cannot fail the whole blob.
			continue
		}
		if err := ValidateModelUserBilling(key, v); err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		v = NormalizeModelPricingOverride(key, v)
		if key == "" || v.IsEmpty() {
			continue
		}
		out[key] = v
	}
	return out, nil
}

// MarshalModelPricingOverridesJSON serializes the override table into the JSON form used for storage.
func MarshalModelPricingOverridesJSON(m map[string]ModelPricingOverride) (string, error) {
	if len(m) == 0 {
		return "{}", nil
	}
	buf, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(buf), nil
}
