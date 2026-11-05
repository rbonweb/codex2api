package proxy

import "strings"

// defaultCodexFallbackModel 是请求未带模型时使用的 Codex 兜底模型。
const defaultCodexFallbackModel = "gpt-5.5"

func canonicalizeCodexModel(model string, supportedModels []string) string {
	trimmed := strings.TrimSpace(model)
	if trimmed == "" {
		return ""
	}
	for _, supported := range supportedModels {
		if strings.EqualFold(trimmed, supported) {
			return supported
		}
	}

	lower := strings.ToLower(trimmed)
	aliases := map[string]string{
		"gpt5-5":       "gpt-5.5",
		"gpt5.5":       "gpt-5.5",
		"gpt5-6-sol":   "gpt-5.6-sol",
		"gpt5.6-sol":   "gpt-5.6-sol",
		"gpt5-6-terra": "gpt-5.6-terra",
		"gpt5.6-terra": "gpt-5.6-terra",
		"gpt5-6-luna":  "gpt-5.6-luna",
		"gpt5.6-luna":  "gpt-5.6-luna",
		"gpt6-astra":   "gpt-6-astra",
	}
	if canonical, ok := aliases[lower]; ok {
		for _, supported := range supportedModels {
			if canonical == supported {
				return canonical
			}
		}
		return trimmed
	}
	return trimmed
}
