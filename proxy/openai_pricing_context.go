package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/codex2api/database"
	"golang.org/x/net/html"
)

var (
	openAILongBinding = regexp.MustCompile(`longContextRows:([A-Za-z_$][\w$]*)`)
	openAIJSKey       = regexp.MustCompile(`([{,])([A-Za-z_$][\w$]*):`)
	openAIJSFraction  = regexp.MustCompile(`:\.(\d+)`)
)

type openAIPriceData map[string]database.ModelPricingOverride

// supplementContext fetches the pricing components referenced by the current page and reads the long-context prices they explicitly publish.
func (prices openAIPriceData) supplementContext(ctx context.Context, client *http.Client, body []byte) error {
	asset := openAIPricingAsset(body)
	if asset == "" {
		return nil
	} // Components are not needed when the regular HTML table already contains all columns.
	script, err := fetchOpenAIPricingAsset(ctx, client, asset)
	if err != nil {
		return err
	}
	rates, err := parseOpenAIContextData(string(script))
	if err != nil {
		return err
	}
	for tier, models := range rates {
		for model, rate := range models {
			price, exists := prices[model]
			if !exists || rate.Input <= 0 || rate.Output <= 0 {
				continue
			}
			if tier == "standard" {
				price.InputLong, price.CachedInputLong, price.OutputLong = rate.Input, rate.CachedInput, rate.Output
				price.LongContextThresholdTokens = openAIPricingLongContextThreshold
			} else if tier == "fast" {
				price.InputLongPriority, price.CachedInputLongPriority, price.OutputLongPriority = rate.Input, rate.CachedInput, rate.Output
			}
			prices[model] = price
		}
	}
	return nil
}

func openAIPricingAsset(body []byte) string {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return ""
	}
	asset := ""
	walkPricingHTML(doc, func(node *html.Node) bool {
		if pricingNodeAttribute(node, "component-export") == "TextTokenPricingTables" {
			candidate := pricingNodeAttribute(node, "component-url")
			parsed, err := url.Parse(candidate)
			if err == nil && parsed.Host == "" && parsed.Scheme == "" && strings.HasPrefix(parsed.Path, "/_astro/pricing.") && strings.HasSuffix(parsed.Path, ".js") {
				asset = "https://developers.openai.com" + candidate
			}
		}
		return true
	})
	return asset
}

func fetchOpenAIPricingAsset(ctx context.Context, client *http.Client, asset string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, officialPricingTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, asset, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", subscriptionsBrowserUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "javascript") {
		return nil, fmt.Errorf("OpenAI pricing component response format is unexpected")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, officialPricingBodyLimit+1))
	if err != nil {
		return nil, err
	}
	if len(body) == 0 || len(body) > officialPricingBodyLimit {
		return nil, fmt.Errorf("OpenAI pricing component size is unexpected")
	}
	return body, nil
}

// parseOpenAIContextData locates the data through the component's longContextRows default binding, decodes only a JSON subset, and never executes JS.
func parseOpenAIContextData(script string) (map[string]map[string]database.ModelPricingOverride, error) {
	binding := openAILongBinding.FindStringSubmatch(script)
	if len(binding) != 2 {
		return nil, fmt.Errorf("OpenAI long-context component structure has changed")
	}
	reference := regexp.MustCompile(`\b` + regexp.QuoteMeta(binding[1]) + `\?\?([A-Za-z_$][\w$]*)\[[^]]+\]\?\?\{\}`).FindStringSubmatch(script)
	if len(reference) != 2 {
		return nil, fmt.Errorf("OpenAI long-context data binding is missing")
	}
	declaration := regexp.MustCompile(`\b` + regexp.QuoteMeta(reference[1]) + `=(\{)`).FindStringSubmatchIndex(script)
	if len(declaration) != 4 {
		return nil, fmt.Errorf("OpenAI long-context data is missing")
	}
	object, err := openAIJSObject(script[declaration[2]:])
	if err != nil {
		return nil, err
	}
	object = openAIJSKey.ReplaceAllString(object, `${1}"${2}":`)
	object = openAIJSFraction.ReplaceAllString(object, `:0.${1}`)
	object = strings.ReplaceAll(object, "`-`", `"-"`)
	return decodeOpenAIContextRates(object)
}

func openAIJSObject(script string) (string, error) {
	depth := 0
	for i, char := range script {
		switch char {
		case '{':
			depth++
		case '}':
			depth--
		}
		if depth == 0 {
			return script[:i+1], nil
		}
	}
	return "", fmt.Errorf("OpenAI long-context data is incomplete")
}

func decodeOpenAIContextRates(object string) (map[string]map[string]database.ModelPricingOverride, error) {
	var raw map[string]map[string]map[string]json.RawMessage
	if err := json.Unmarshal([]byte(object), &raw); err != nil {
		return nil, fmt.Errorf("OpenAI long-context data format is unexpected: %w", err)
	}
	out := make(map[string]map[string]database.ModelPricingOverride)
	for _, tier := range []string{"standard", "fast"} {
		if len(raw[tier]) == 0 {
			return nil, fmt.Errorf("OpenAI long-context tier is missing: %s", tier)
		}
		out[tier] = make(map[string]database.ModelPricingOverride)
		for model, fields := range raw[tier] {
			price, err := decodeOpenAIContextRate(fields)
			if err != nil {
				return nil, err
			}
			out[tier][model] = price
		}
	}
	return out, nil
}

func decodeOpenAIContextRate(fields map[string]json.RawMessage) (database.ModelPricingOverride, error) {
	row := officialPricingRow{}
	for _, key := range []string{"input", "cachedInput", "output"} {
		value := strings.TrimSpace(string(fields[key]))
		if value == `"-"` {
			row[key] = "-"
		} else {
			row[key] = "$" + value
		}
	}
	price := database.ModelPricingOverride{Input: row.price("input"), CachedInput: row.price("cachedInput"), Output: row.price("output")}
	return price, validateSyncedPricing("", price, false)
}
