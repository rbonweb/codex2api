package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/codex2api/database"
	"golang.org/x/net/html"
)

const (
	openAILatestPriceColumns = 5
	openAIOlderPriceColumns  = 4
)

// openAIPricingIslands reads the data the official site uses to render its collapsible tables, without executing page scripts.
func openAIPricingIslands(body []byte, out map[string]database.ModelPricingOverride) error {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return err
	}
	var parseErr error
	walkPricingHTML(doc, func(node *html.Node) bool {
		if node.Data != "astro-island" || pricingNodeAttribute(node, "component-export") != "TextTokenPricingTables" {
			return true
		}
		var props map[string]json.RawMessage
		if err := json.Unmarshal([]byte(pricingNodeAttribute(node, "props")), &props); err != nil {
			parseErr = err
			return false
		}
		if err := openAIIslandRows(props, out); err != nil {
			parseErr = err
		}
		return false
	})
	return parseErr
}

func pricingNodeAttribute(node *html.Node, key string) string {
	for _, attr := range node.Attr {
		if attr.Key == key {
			return attr.Val
		}
	}
	return ""
}

// decodePricingProp only supports Astro's plain value and array encodings; unknown encodings are rejected directly.
func decodePricingProp(raw json.RawMessage) (any, error) {
	var pair []json.RawMessage
	if err := json.Unmarshal(raw, &pair); err != nil || len(pair) != 2 {
		return nil, fmt.Errorf("OpenAI page data encoding is unexpected")
	}
	var kind int
	if err := json.Unmarshal(pair[0], &kind); err != nil {
		return nil, err
	}
	if kind == 0 {
		var value any
		err := json.Unmarshal(pair[1], &value)
		return value, err
	}
	if kind != 1 {
		return nil, fmt.Errorf("OpenAI page data encoding type is not supported")
	}
	var items []json.RawMessage
	if err := json.Unmarshal(pair[1], &items); err != nil {
		return nil, err
	}
	values := make([]any, 0, len(items))
	for _, item := range items {
		value, err := decodePricingProp(item)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, nil
}

func openAIIslandRows(props map[string]json.RawMessage, out map[string]database.ModelPricingOverride) error {
	tier, err := decodePricingProp(props["tier"])
	if err != nil {
		return err
	}
	if tier != "standard" && tier != "fast" {
		return nil
	}
	decoded, err := decodePricingProp(props["rows"])
	if err != nil {
		return err
	}
	rows, ok := decoded.([]any)
	if !ok {
		return fmt.Errorf("OpenAI rows format is unexpected")
	}
	table := officialHTMLPricingTable{tier: tier.(string)}
	for _, value := range rows {
		row, err := openAIIslandRow(value)
		if err != nil {
			return err
		}
		table.rows = append(table.rows, row)
	}
	return parseOpenAIPricingTable(table, out)
}

func openAIIslandRow(value any) (officialPricingRow, error) {
	cells, ok := value.([]any)
	if !ok || (len(cells) != openAIOlderPriceColumns && len(cells) != openAILatestPriceColumns) {
		return nil, fmt.Errorf("OpenAI text price data column count is unexpected")
	}
	model, ok := cells[0].(string)
	if !ok || !strings.HasPrefix(model, "gpt-") && !strings.HasPrefix(model, "o") {
		return nil, fmt.Errorf("OpenAI model column format is unexpected")
	}
	row := officialPricingRow{"model": model}
	for key, index := range map[string]int{"input": 1, "cached input": 2, "output": len(cells) - 1} {
		switch value := cells[index].(type) {
		case nil:
			if key != "cached input" {
				return nil, fmt.Errorf("OpenAI required price data is missing")
			}
			row[key] = "-"
		case float64:
			row[key] = "$" + strconv.FormatFloat(value, 'f', -1, 64)
		case string:
			if value != "-" {
				return nil, fmt.Errorf("OpenAI price data is not a number")
			}
			row[key] = value
		default:
			return nil, fmt.Errorf("OpenAI price data type is unexpected")
		}
	}
	return row, nil
}
