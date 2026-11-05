package proxy

import (
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

// The Codex upstream requires function/custom tool names to match ^[a-zA-Z0-9_-]{1,64}$. Chat Completions
// Clients (especially agent frameworks that pass MCP tool names through unchanged) often include `.`, `:`, spaces, or non-ASCII characters,
// forwarding them unchanged always returns 400 "Invalid tools[N].name". Here, during the Chat→Responses conversion, illegal
// characters are replaced with `_`, over-long names are truncated and de-duplicated, and the response side restores the names using the same mapping, so the client still sees
// the original names. Valid names are not added to the mapping, so regular requests have zero overhead.
//
// Only applies to the /v1/chat/completions route: native Responses requests are treated as already built to the upstream spec,
// The Grok route has its own namespace mapping.
const codexToolNameLimit = 64

func isCodexToolNameRune(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-'
}

// sanitizeCodexToolName replaces characters outside [a-zA-Z0-9_-] with underscores.
func sanitizeCodexToolName(name string) string {
	changed := false
	var sb strings.Builder
	for _, r := range name {
		if isCodexToolNameRune(r) {
			sb.WriteRune(r)
			continue
		}
		sb.WriteByte('_')
		changed = true
	}
	if !changed {
		return name
	}
	return sb.String()
}

// shortenCodexToolName sanitizes the name, then truncates it to 64 bytes; names with the mcp__ prefix keep the prefix and
// the last segment (the server name segment is the longest and the least distinctive). After sanitizing, everything is ASCII, so byte-level slicing is safe.
func shortenCodexToolName(name string) string {
	sanitized := sanitizeCodexToolName(name)
	if len(sanitized) <= codexToolNameLimit {
		return sanitized
	}
	if strings.HasPrefix(sanitized, "mcp__") {
		if idx := strings.LastIndex(sanitized, "__"); idx > 0 {
			candidate := "mcp__" + sanitized[idx+2:]
			if len(candidate) > codexToolNameLimit {
				candidate = candidate[:codexToolNameLimit]
			}
			return candidate
		}
	}
	return sanitized[:codexToolNameLimit]
}

// collectChatToolNames collects all tool names that appear in the request, in a deterministic order: the tools declaration,
// tool_choice, and historical assistant tool_calls. Renaming on the request side and restoring on the response side both work from the same
// list, so the de-duplication suffixes stay consistent on both sides.
func collectChatToolNames(req openAIRequest) []string {
	var names []string
	seen := map[string]struct{}{}
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	for _, raw := range req.Tools {
		tool := gjson.ParseBytes(raw)
		switch tool.Get("type").String() {
		case "function", "":
			name := tool.Get("function.name").String()
			if name == "" {
				name = tool.Get("name").String()
			}
			add(name)
		case "custom":
			name := tool.Get("custom.name").String()
			if name == "" {
				name = tool.Get("name").String()
			}
			add(name)
		}
	}
	if choice := gjson.ParseBytes(req.ToolChoice); choice.IsObject() {
		switch choice.Get("type").String() {
		case "function":
			name := choice.Get("function.name").String()
			if name == "" {
				name = choice.Get("name").String()
			}
			add(name)
		case "custom":
			name := choice.Get("custom.name").String()
			if name == "" {
				name = choice.Get("name").String()
			}
			add(name)
		}
	}
	for _, msg := range req.Messages {
		if msg.Role != "assistant" {
			continue
		}
		for _, tc := range msg.ToolCalls {
			if tc.Type == "custom" && tc.Custom != nil {
				add(tc.Custom.Name)
				continue
			}
			add(tc.Function.Name)
		}
	}
	return names
}

// buildCodexToolNameMap builds the original name → upstream name mapping, and only records names that need rewriting.
// Valid names are reserved first, then de-duplication suffixes are assigned to invalid names, so valid names are not pushed out.
func buildCodexToolNameMap(names []string) map[string]string {
	used := make(map[string]struct{}, len(names))
	var pending []string
	for _, name := range names {
		if shortenCodexToolName(name) == name {
			used[name] = struct{}{}
			continue
		}
		pending = append(pending, name)
	}
	if len(pending) == 0 {
		return nil
	}
	mapped := make(map[string]string, len(pending))
	for _, name := range pending {
		short := uniqueCodexToolName(shortenCodexToolName(name), used)
		used[short] = struct{}{}
		mapped[name] = short
	}
	return mapped
}

func uniqueCodexToolName(base string, used map[string]struct{}) string {
	if _, taken := used[base]; !taken {
		return base
	}
	for i := 1; ; i++ {
		suffix := "_" + strconv.Itoa(i)
		candidate := base
		if len(candidate)+len(suffix) > codexToolNameLimit {
			candidate = candidate[:codexToolNameLimit-len(suffix)]
		}
		candidate += suffix
		if _, taken := used[candidate]; !taken {
			return candidate
		}
	}
}

// applyCodexToolNameMap applies the mapping to the converted Responses request body: the tools declaration,
// tool_choice, and the historical function_call / custom_tool_call items in input.
func applyCodexToolNameMap(out map[string]any, nameMap map[string]string) {
	if len(nameMap) == 0 || out == nil {
		return
	}
	rename := func(item map[string]any) {
		name, ok := item["name"].(string)
		if !ok {
			return
		}
		if short, ok := nameMap[name]; ok {
			item["name"] = short
		}
	}
	if tools, ok := out["tools"].([]any); ok {
		for _, raw := range tools {
			tool, ok := raw.(map[string]any)
			if !ok || isReservedCodexTool(tool) {
				continue
			}
			switch strings.TrimSpace(firstNonEmptyAnyString(tool["type"])) {
			case "function", "custom":
				rename(tool)
			}
		}
	}
	if choice, ok := out["tool_choice"].(map[string]any); ok {
		rename(choice)
	}
	if input, ok := out["input"].([]any); ok {
		for _, raw := range input {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			switch strings.TrimSpace(firstNonEmptyAnyString(item["type"])) {
			case "function_call", "custom_tool_call":
				rename(item)
			}
		}
	}
}

// ChatToolNameRestoreMap returns the restore mapping from upstream name → original name. It returns nil when the request has no
// names that need rewriting. It is derived from the same parse result as TranslateRequest.
func ChatToolNameRestoreMap(rawJSON []byte) map[string]string {
	nameMap := buildCodexToolNameMap(collectChatToolNames(cachedOrParse(rawJSON)))
	if len(nameMap) == 0 {
		return nil
	}
	restore := make(map[string]string, len(nameMap))
	for original, short := range nameMap {
		restore[short] = original
	}
	return restore
}

func restoreCodexToolName(restore map[string]string, name string) string {
	if len(restore) == 0 {
		return name
	}
	if original, ok := restore[name]; ok {
		return original
	}
	return name
}

// restoreToolCallNames restores the tool call names in a non-streaming Chat response to the client's original names.
func restoreToolCallNames(toolCalls []ToolCallResult, restore map[string]string) []ToolCallResult {
	if len(restore) == 0 {
		return toolCalls
	}
	for i := range toolCalls {
		toolCalls[i].Name = restoreCodexToolName(restore, toolCalls[i].Name)
	}
	return toolCalls
}
