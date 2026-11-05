package proxy

import (
	"fmt"

	"github.com/codex2api/database"
)

const openAIPricingLongContextThreshold = 272000

// ParseOpenAIOfficialPricingHTML reads only the HTML-named columns and the explicit Standard/Fast tiers.
func ParseOpenAIOfficialPricingHTML(body []byte) (map[string]database.ModelPricingOverride, error) {
	tables, err := parseOfficialPricingHTML(body)
	if err != nil {
		return nil, err
	}
	out := make(map[string]database.ModelPricingOverride)
	for _, table := range tables {
		if table.tier != "standard" && table.tier != "fast" && table.tier != "priority" {
			continue
		}
		if err := parseOpenAIPricingTable(table, out); err != nil {
			return nil, err
		}
	}
	if err := openAIPricingIslands(body, out); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no valid OpenAI HTML pricing table found")
	}
	return out, nil
}

// parseOpenAIPricingTable distinguishes text and image prices by column name, and excludes training fees and other units.
func parseOpenAIPricingTable(table officialHTMLPricingTable, out map[string]database.ModelPricingOverride) error {
	for _, row := range table.rows {
		model := normalizeOfficialPricingModel(row.cell("model"))
		if model == "" || row.cell("training", "use case", "estimated cost") != "" {
			continue
		}
		if row.cell("modality") != "" {
			applyOfficialImagePricingRow(out, row)
			continue
		}
		if row.cell("short context input", "input") == "" || row.cell("short context output", "output") == "" {
			continue
		}
		if row.cell("short context cached input", "cached input") == "" {
			return fmt.Errorf("OpenAI pricing table is missing the cache read column")
		}
		price := out[model]
		if table.tier == "fast" || table.tier == "priority" {
			price = openAIFastPricing(price, row)
		} else {
			price = mergeOpenAIStandardPricing(price, row)
		}
		out[model] = price
	}
	return nil
}

// mergeOpenAIStandardPricing updates the standard tier and keeps the Fast fields already read.
func mergeOpenAIStandardPricing(price database.ModelPricingOverride, row officialPricingRow) database.ModelPricingOverride {
	standard := openAIStandardPricing(row)
	price.Input, price.CachedInput, price.Output = standard.Input, standard.CachedInput, standard.Output
	price.InputLong, price.CachedInputLong, price.OutputLong = standard.InputLong, standard.CachedInputLong, standard.OutputLong
	price.LongContextThresholdTokens = standard.LongContextThresholdTokens
	return price
}

// openAIStandardPricing maps the standard tier header names to the price fields the system uses.
func openAIStandardPricing(row officialPricingRow) database.ModelPricingOverride {
	override := database.ModelPricingOverride{
		Input:           row.price("short context input", "input"),
		CachedInput:     row.price("short context cached input", "cached input"),
		Output:          row.price("short context output", "output"),
		InputLong:       row.price("long context input"),
		CachedInputLong: row.price("long context cached input"),
		OutputLong:      row.price("long context output"),
	}
	if override.InputLong > 0 || override.OutputLong > 0 {
		override.LongContextThresholdTokens = openAIPricingLongContextThreshold
	}
	return override
}

// openAIFastPricing merges the separately published Fast prices without relying on fixed column numbers or inferring from multipliers.
func openAIFastPricing(override database.ModelPricingOverride, row officialPricingRow) database.ModelPricingOverride {
	override.InputPriority = row.price("short context input", "input")
	override.CachedInputPriority = row.price("short context cached input", "cached input")
	override.OutputPriority = row.price("short context output", "output")
	override.InputLongPriority = row.price("long context input")
	override.CachedInputLongPriority = row.price("long context cached input")
	override.OutputLongPriority = row.price("long context output")
	return override
}
