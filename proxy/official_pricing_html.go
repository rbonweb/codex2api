package proxy

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

const maxPricingTableSpan = 32

type officialPricingRow map[string]string

type officialHTMLPricingTable struct {
	rows []officialPricingRow
	tier string
}

type pricingSpan struct {
	node *html.Node
	left int
}

// parseOfficialPricingHTML reads the table from the page and validates its merged-cell structure. Markdown is not accepted.
func parseOfficialPricingHTML(body []byte) ([]officialHTMLPricingTable, error) {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	var tables []officialHTMLPricingTable
	var parseErr error
	walkPricingHTML(doc, func(node *html.Node) bool {
		if node.Type != html.ElementNode || node.Data != "table" {
			return true
		}
		grid, err := pricingHTMLGrid(node)
		if err != nil {
			parseErr = err
			return false
		}
		tables = append(tables, officialHTMLPricingTable{rows: pricingHTMLRows(grid), tier: pricingHTMLTier(node)})
		return false
	})
	if parseErr != nil {
		return nil, parseErr
	}
	if len(tables) == 0 {
		return nil, fmt.Errorf("official page format is unexpected: no HTML pricing table found")
	}
	return tables, nil
}

// pricingHTMLGrid expands rowspan/colspan and rejects data that overlaps, exceeds limits, or is missing columns.
func pricingHTMLGrid(table *html.Node) ([][]*html.Node, error) {
	var grid [][]*html.Node
	var spans [maxPricingTableSpan]pricingSpan
	for _, cells := range pricingHTMLTableRows(table) {
		row := make([]*html.Node, maxPricingTableSpan)
		for i := range spans {
			if spans[i].left > 0 {
				row[i] = spans[i].node
				spans[i].left--
			}
		}
		if err := placePricingHTMLCells(row, spans[:], cells); err != nil {
			return nil, err
		}
		for len(row) > 0 && row[len(row)-1] == nil {
			row = row[:len(row)-1]
		}
		grid = append(grid, row)
	}
	return grid, nil
}

// placePricingHTMLCells places this row's data around the cells that already span across rows.
func placePricingHTMLCells(row []*html.Node, spans []pricingSpan, cells []*html.Node) error {
	column := 0
	for _, cell := range cells {
		for column < len(row) && row[column] != nil {
			column++
		}
		width, height := pricingHTMLSpan(cell, "colspan"), pricingHTMLSpan(cell, "rowspan")
		if width <= 0 || height <= 0 || column+width > len(row) {
			return fmt.Errorf("official pricing table has malformed merged cells")
		}
		for i := column; i < column+width; i++ {
			if row[i] != nil {
				return fmt.Errorf("official pricing table cells overlap")
			}
			row[i] = cell
			spans[i] = pricingSpan{node: cell, left: height - 1}
		}
		column += width
	}
	return nil
}

// pricingHTMLSpan only accepts bounded positive integer spans.
func pricingHTMLSpan(node *html.Node, key string) int {
	value := pricingHTMLAttribute(node, key)
	if value == "" {
		return 1
	}
	span, err := strconv.Atoi(value)
	if err != nil || span > maxPricingTableSpan {
		return 0
	}
	return span
}

// pricingHTMLRows merges multi-row headers into column names, keeping the meaning of each column.
func pricingHTMLRows(grid [][]*html.Node) []officialPricingRow {
	var header []string
	var rows []officialPricingRow
	for _, cells := range grid {
		if pricingHTMLHeaderRow(cells) {
			header = mergePricingHTMLHeader(header, cells)
			continue
		}
		if len(header) == 0 || len(cells) != len(header) {
			continue
		}
		row := make(officialPricingRow, len(header))
		for i, cell := range cells {
			text := htmlNodeText(cell)
			if header[i] == "model" || header[i] == "name" {
				text = pricingHTMLModelText(cell)
			}
			row[header[i]] = strings.TrimSpace(text)
		}
		rows = append(rows, row)
	}
	return rows
}

// pricingHTMLHeaderRow reports whether the row consists entirely of header cells.
func pricingHTMLHeaderRow(cells []*html.Node) bool {
	for _, cell := range cells {
		if cell == nil || cell.Data != "th" {
			return false
		}
	}
	return len(cells) > 0
}

// mergePricingHTMLHeader merges group names and drops group prefixes that do not affect the pricing meaning.
func mergePricingHTMLHeader(header []string, cells []*html.Node) []string {
	if len(header) < len(cells) {
		header = append(header, make([]string, len(cells)-len(header))...)
	}
	for i, cell := range cells {
		label := strings.ToLower(strings.Join(strings.Fields(htmlNodeText(cell)), " "))
		if header[i] != "" && label != "" && label != header[i] {
			label = header[i] + " " + label
		}
		label = strings.TrimPrefix(label, "base tokens ")
		label = strings.TrimPrefix(label, "prompt caching ")
		if label == "model name" {
			label = "model"
		}
		header[i] = label
	}
	return header
}

// pricingHTMLTier reads the nearest tier marker so that Batch/Ultrafast prices do not overwrite standard prices.
func pricingHTMLTier(node *html.Node) string {
	for current := node; current != nil; current = current.Parent {
		for _, key := range []string{"data-pricing-tier", "data-value"} {
			value := pricingHTMLAttribute(current, key)
			switch value {
			case "standard", "fast", "priority", "batch", "flex", "ultrafast":
				return value
			}
		}
	}
	return "standard"
}

// pricingHTMLAttribute returns an HTML attribute value.
func pricingHTMLAttribute(node *html.Node, key string) string {
	for _, attribute := range node.Attr {
		if attribute.Key == key {
			return attribute.Val
		}
	}
	return ""
}

// cell reads values only from explicitly named columns and never infers prices from column position.
func (row officialPricingRow) cell(columns ...string) string {
	for _, column := range columns {
		if value, ok := row[column]; ok {
			return value
		}
	}
	return ""
}

// price reads the USD unit price from a named column; invalid content returns NaN, which price validation rejects.
func (row officialPricingRow) price(columns ...string) float64 {
	return parseOfficialPrice(row.cell(columns...))
}

// normalizeOfficialPricingHeader normalizes the case and whitespace of old single-row headers.
func normalizeOfficialPricingHeader(cells []string) []string {
	header := make([]string, len(cells))
	for i, cell := range cells {
		header[i] = strings.ToLower(strings.Join(strings.Fields(cell), " "))
	}
	return header
}

// walkPricingHTML walks the nodes; when the callback returns false, it does not visit that node's subtree.
func walkPricingHTML(node *html.Node, visit func(*html.Node) bool) {
	if !visit(node) {
		return
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		walkPricingHTML(child, visit)
	}
}

// pricingHTMLTableRows collects the cells of an HTML table and keeps the nodes so that model links can be told apart from description text.
func pricingHTMLTableRows(table *html.Node) [][]*html.Node {
	var rows [][]*html.Node
	walkPricingHTML(table, func(node *html.Node) bool {
		if node.Type != html.ElementNode || node.Data != "tr" {
			return true
		}
		var cells []*html.Node
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			if child.Type == html.ElementNode && (child.Data == "th" || child.Data == "td") {
				cells = append(cells, child)
			}
		}
		rows = append(rows, cells)
		return false
	})
	return rows
}

// pricingHTMLModelText reads the model link first, so that the description and status icon are not concatenated into the model ID.
func pricingHTMLModelText(cell *html.Node) string {
	model := ""
	walkPricingHTML(cell, func(node *html.Node) bool {
		if model == "" && node.Type == html.ElementNode && node.Data == "a" {
			label := strings.TrimSpace(htmlNodeText(node))
			if isOfficialPricingModelLabel(label) {
				model = label
			}
		}
		return model == ""
	})
	if model != "" {
		return model
	}
	return htmlNodeText(cell)
}

// isOfficialPricingModelLabel only treats model links as names and excludes help links and descriptions.
func isOfficialPricingModelLabel(label string) bool {
	label = strings.ToLower(label)
	return strings.HasPrefix(label, "gpt-")
}

// htmlNodeText extracts the visible text of a cell.
func htmlNodeText(node *html.Node) string {
	if node == nil {
		return ""
	}
	if node.Type == html.TextNode {
		return node.Data
	}
	for _, attribute := range node.Attr {
		if attribute.Key == "aria-hidden" && attribute.Val == "true" {
			return ""
		}
	}
	var text strings.Builder
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		text.WriteString(htmlNodeText(child))
	}
	return text.String()
}
