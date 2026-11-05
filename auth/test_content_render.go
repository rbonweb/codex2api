package auth

import (
	"math/rand/v2"
	"strconv"
	"strings"
	"time"
)

// ==================== Liveness probe content rendering (issue #320) ====================
//
// test_content supports two layers of randomization, to reduce the fingerprint of "a batch of accounts all sending the same liveness probe content":
//  1. Multiple lines = candidate pool: split by line (empty lines filtered out), and each probe randomly picks one line.
//     Single-line input behaves exactly as before, so it is backward compatible.
//  2. Variable templates: {{...}} placeholders in the picked line are expanded before sending:
//     {{time}}           current time 15:04:05
//     {{date}}           current date 2006-01-02
//     {{datetime}}       2006-01-02 15:04:05
//     {{timestamp}}      Unix timestamp in seconds
//     {{rand}}           random integer from 0-9999
//     {{rand:min-max}}   random integer in the range [min,max] (swapped automatically when min>max)
//     Unknown variables are kept as written (liveness probe content is free text, so they are handled leniently without an error).

// RenderTestContent renders the configured liveness probe content into the text actually sent for this probe:
// it randomly picks a candidate line and expands variables. Empty input falls back to DefaultTestContent.
func RenderTestContent(raw string) string {
	line := pickTestContentLine(raw)
	if line == "" {
		return DefaultTestContent
	}
	return expandTestContentVariables(line)
}

// TestContentLines returns the non-empty candidate lines after splitting by line (for validation and preview).
func TestContentLines(raw string) []string {
	var lines []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func pickTestContentLine(raw string) string {
	lines := TestContentLines(raw)
	switch len(lines) {
	case 0:
		return ""
	case 1:
		return lines[0]
	default:
		return lines[rand.IntN(len(lines))]
	}
}

func expandTestContentVariables(line string) string {
	if !strings.Contains(line, "{{") {
		return line
	}
	var b strings.Builder
	b.Grow(len(line))
	for {
		start := strings.Index(line, "{{")
		if start < 0 {
			b.WriteString(line)
			break
		}
		end := strings.Index(line[start+2:], "}}")
		if end < 0 {
			b.WriteString(line)
			break
		}
		b.WriteString(line[:start])
		token := line[start+2 : start+2+end]
		if value, ok := resolveTestContentVariable(token); ok {
			b.WriteString(value)
		} else {
			// Unknown variables are kept as written, including the braces
			b.WriteString(line[start : start+2+end+2])
		}
		line = line[start+2+end+2:]
	}
	return b.String()
}

func resolveTestContentVariable(token string) (string, bool) {
	token = strings.TrimSpace(token)
	now := time.Now()
	switch strings.ToLower(token) {
	case "time":
		return now.Format("15:04:05"), true
	case "date":
		return now.Format("2006-01-02"), true
	case "datetime":
		return now.Format("2006-01-02 15:04:05"), true
	case "timestamp":
		return strconv.FormatInt(now.Unix(), 10), true
	case "rand":
		return strconv.Itoa(rand.IntN(10000)), true
	}
	if rangeSpec, ok := strings.CutPrefix(strings.ToLower(token), "rand:"); ok {
		if value, ok := randInRange(rangeSpec); ok {
			return value, true
		}
	}
	return "", false
}

// randInRange parses "min-max" and returns a random integer in the range. Negative lower bounds (such as "-5-5")
// are not supported: the separator is ambiguous and liveness probes never need them, so a parse failure keeps the token as an unknown variable.
func randInRange(spec string) (string, bool) {
	minStr, maxStr, ok := strings.Cut(strings.TrimSpace(spec), "-")
	if !ok {
		return "", false
	}
	minV, err1 := strconv.Atoi(strings.TrimSpace(minStr))
	maxV, err2 := strconv.Atoi(strings.TrimSpace(maxStr))
	if err1 != nil || err2 != nil || minV < 0 {
		return "", false
	}
	if minV > maxV {
		minV, maxV = maxV, minV
	}
	return strconv.Itoa(minV + rand.IntN(maxV-minV+1)), true
}
