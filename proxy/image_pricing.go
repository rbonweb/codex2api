package proxy

import (
	"strings"

	"github.com/codex2api/database"
)

// applyOfficialImagePricingRow reads only supported image models and merges the Image and Text rows separately.
func applyOfficialImagePricingRow(out map[string]database.ModelPricingOverride, row officialPricingRow) {
	model := database.GPTImage25BillingModel(normalizeOfficialPricingModel(row.cell("model")))
	if model == "" {
		return
	}
	price := out[model]
	switch strings.ToLower(row.cell("modality")) {
	case "image":
		price.ImageInput, price.CachedImageInput, price.Output = row.price("input"), row.price("cached input"), row.price("output")
	case "text":
		price.Input, price.CachedInput = row.price("input"), row.price("cached input")
	default:
		return
	}
	out[model] = price
}

// projectImageResolutionPricing gives each independently editable resolution variant the base model's token price.
// The official pricing does not vary the token price by output resolution; exact model prices and user-defined prices still take precedence.
func projectImageResolutionPricing(allowed map[string]struct{}, parsed map[string]database.ModelPricingOverride) {
	base, exists := parsed["gpt-image-2"]
	if !exists {
		return
	}
	for _, model := range []string{"gpt-image-2-2k", "gpt-image-2-4k"} {
		if _, enabled := allowed[model]; !enabled {
			continue
		}
		if _, exact := parsed[model]; !exact {
			parsed[model] = base
		}
	}
}
