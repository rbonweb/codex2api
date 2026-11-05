package proxy

import (
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/codex2api/database"
)

var officialDollarPrice = regexp.MustCompile(`^\$(-?(?:\d{1,3}(?:,\d{3})+|\d+)(?:\.\d+)?)(?:\s|/|$)`)

var officialPriceUnit = regexp.MustCompile(`(?i)^/\s*(?:mtok|1m\s+tokens|million\s+tokens|image|img|sec|second)(?:\s|$)`)

// normalizeOfficialPricingModel 去除上下文注释并保留模型的独立价格键。
func normalizeOfficialPricingModel(value string) string {
	value = strings.ToLower(strings.Trim(strings.TrimSpace(value), "`"))
	if idx := strings.Index(value, " ("); idx >= 0 {
		value = value[:idx]
	}
	return database.PricingManagementModelKey(value)
}

// parseOfficialPrice 严格读取美元数值，未知格式返回 NaN，禁止静默当成零价。
func parseOfficialPrice(value string) float64 {
	value = strings.TrimSpace(value)
	if value == "" || value == "-" || value == "—" {
		return 0
	}
	match := officialDollarPrice.FindStringSubmatch(value)
	if len(match) < 2 {
		return math.NaN()
	}
	unit := strings.TrimSpace(value[len(match[1])+1:])
	if !validOfficialPriceUnit(unit) {
		return math.NaN()
	}
	price, err := strconv.ParseFloat(strings.ReplaceAll(match[1], ",", ""), 64)
	if err != nil {
		return math.NaN()
	}
	return price
}

// validOfficialPriceUnit 拒绝按千 token、分钟或字符计费的价格混入 token/媒体单价。
func validOfficialPriceUnit(unit string) bool {
	if unit == "" || strings.EqualFold(strings.Join(strings.Fields(unit), ""), "/1m") {
		return true
	}
	return officialPriceUnit.MatchString(unit)
}

// uniqueSortedStrings 返回去空白、去重且按字典序排列的字符串。
func uniqueSortedStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
