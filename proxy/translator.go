package proxy

import (
	"container/list"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/proxy/internal/jsonbody"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ==================== input structs (OpenAI Chat Completions format) ====================

// openAIRequest represents an OpenAI Chat Completions request (only the fields needed for translation are parsed)
type openAIRequest struct {
	Model               string            `json:"model"`
	Messages            []openAIMessage   `json:"messages"`
	Tools               []json.RawMessage `json:"tools"`
	ResponseFormat      json.RawMessage   `json:"response_format,omitempty"`
	ReasoningEffort     string            `json:"reasoning_effort"`
	ServiceTier         string            `json:"service_tier"`
	ServiceTierAlt      string            `json:"serviceTier"` // compatible with camelCase naming
	Temperature         json.RawMessage   `json:"temperature,omitempty"`
	TopP                json.RawMessage   `json:"top_p,omitempty"`
	MaxTokens           json.RawMessage   `json:"max_tokens,omitempty"`
	MaxCompletionTokens json.RawMessage   `json:"max_completion_tokens,omitempty"`
	Stop                json.RawMessage   `json:"stop,omitempty"`
	Seed                json.RawMessage   `json:"seed,omitempty"`
	PresencePenalty     json.RawMessage   `json:"presence_penalty,omitempty"`
	FrequencyPenalty    json.RawMessage   `json:"frequency_penalty,omitempty"`
	ParallelToolCalls   json.RawMessage   `json:"parallel_tool_calls,omitempty"`
	ToolChoice          json.RawMessage   `json:"tool_choice,omitempty"`
}

// openAIMessage represents one OpenAI message
type openAIMessage struct {
	Role       string           `json:"role"`
	Content    json.RawMessage  `json:"content"` // string or []contentPart
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

// openAIToolCall represents a tool call in an assistant message
type openAIToolCall struct {
	Type      string              `json:"type"`
	ID        string              `json:"id"`
	Custom    *customToolCallData `json:"custom,omitempty"`
	Namespace string              `json:"namespace,omitempty"`
	Function  struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// openAIToolParsed represents a parsed tool definition
type openAIToolParsed struct {
	Type     string          `json:"type"`
	Function *openAIToolFunc `json:"function,omitempty"`
}

// openAIToolFunc represents the function description of a tool
type openAIToolFunc struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

// openAIContentPart represents one item in multi-part content
type openAIContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url,omitempty"`
	File *struct {
		Filename string `json:"filename,omitempty"`
		FileData string `json:"file_data,omitempty"`
		FileID   string `json:"file_id,omitempty"`
	} `json:"file,omitempty"`
}

// ==================== output structs (OpenAI streaming / non-streaming response format) ====================

// openAIStreamChunk is a streaming response chunk
type openAIStreamChunk struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []streamChoice `json:"choices"`
	Usage   *UsageInfo     `json:"usage,omitempty"`
	// ServiceTier returns the upstream's actual processing tier (response.service_tier); it is only carried in the final chunk.
	ServiceTier string `json:"service_tier,omitempty"`
}

// streamChoice is a choice in a streaming chunk
type streamChoice struct {
	Index        int          `json:"index"`
	Delta        *streamDelta `json:"delta,omitempty"`
	FinishReason *string      `json:"finish_reason"`
}

// streamDelta is the incremental content of a streaming chunk.
//
// The reasoning field is emitted under two names to stay compatible with different clients:
//   - reasoning:  OpenAI official o1/GPT-5 style (Cherry Studio and others use this by default)
//   - reasoning_content: style used by DeepSeek / OpenRouter / new-api and other clone sites
type streamDelta struct {
	Role             string          `json:"role,omitempty"`
	Content          *string         `json:"content,omitempty"`
	Reasoning        *string         `json:"reasoning,omitempty"`
	ReasoningContent *string         `json:"reasoning_content,omitempty"`
	ToolCalls        []toolCallDelta `json:"tool_calls,omitempty"`
}

// toolCallDelta is an incremental tool call update
type toolCallDelta struct {
	Index     int                 `json:"index"`
	ID        string              `json:"id,omitempty"`
	Type      string              `json:"type,omitempty"`
	Function  *toolCallFuncDelta  `json:"function,omitempty"`
	Custom    *customToolCallData `json:"custom,omitempty"`
	Namespace string              `json:"namespace,omitempty"`
}

// toolCallFuncDelta is an incremental tool function update
type toolCallFuncDelta struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

// openAICompactResponse is a complete non-streaming response
type openAICompactResponse struct {
	ID      string          `json:"id"`
	Object  string          `json:"object"`
	Created int64           `json:"created,omitempty"`
	Model   string          `json:"model"`
	Choices []compactChoice `json:"choices"`
	Usage   *UsageInfo      `json:"usage,omitempty"`
	// ServiceTier returns the upstream's actual processing tier (response.service_tier).
	ServiceTier string `json:"service_tier,omitempty"`
}

// compactChoice is a choice in a non-streaming response
type compactChoice struct {
	Index        int            `json:"index"`
	Message      compactMessage `json:"message"`
	FinishReason string         `json:"finish_reason"`
}

// compactMessage is a message in a non-streaming response. reasoning / reasoning_content are both emitted for multi-client compatibility.
type compactMessage struct {
	Role             string               `json:"role"`
	Content          *string              `json:"content"`
	Reasoning        *string              `json:"reasoning,omitempty"`
	ReasoningContent *string              `json:"reasoning_content,omitempty"`
	ToolCalls        []compactToolCallOut `json:"tool_calls,omitempty"`
}

// compactToolCallOut is a tool call in a non-streaming response
type compactToolCallOut struct {
	ID        string              `json:"id"`
	Type      string              `json:"type"`
	Function  *toolCallFuncDelta  `json:"function,omitempty"`
	Custom    *customToolCallData `json:"custom,omitempty"`
	Namespace string              `json:"namespace,omitempty"`
}

// openAIErrorResponse is an error response
type openAIErrorResponse struct {
	Error struct {
		Message string          `json:"message"`
		Type    string          `json:"type"`
		Details json.RawMessage `json:"details,omitempty"`
	} `json:"error"`
}

// ==================== LRU request parse cache ====================

const (
	requestCacheSize          = 256
	requestCacheMaxBytes      = 64 << 20
	requestCacheMaxEntryBytes = 1 << 20
	requestCacheTTL           = 10 * time.Minute
)

// maxTools is the maximum number of tools allowed by the upstream Codex API
const maxTools = 128

const (
	codexImageGenerationBridgeMarker = "<codex2api-codex-image-generation>"
	codexImageGenerationBridgeText   = codexImageGenerationBridgeMarker + "\nWhen the user asks for raster image generation or editing, use the OpenAI Responses native `image_generation` tool attached to this request. The local Codex client may not expose an `image_gen` namespace, but that does not mean image generation is unavailable. Do not ask the user to switch to CLI fallback solely because `image_gen` is absent.\n</codex2api-codex-image-generation>"
	jsonObjectFormatInputHint        = "Return a valid JSON object."
)

var responsesImageGenerationOptionFields = []string{
	"size",
	"quality",
	"background",
	"output_format",
	"output_compression",
	"moderation",
	"partial_images",
}

var responsesImageGenerationUnsupportedOptionFields = []string{
	"style",
}

type requestCacheEntry struct {
	key       [32]byte
	req       openAIRequest
	size      int
	expiresAt time.Time
}

type requestCache struct {
	mu            sync.Mutex
	order         *list.List
	items         map[[32]byte]*list.Element
	bytes         int
	maxBytes      int
	maxEntryBytes int
	ttl           time.Duration
}

var globalRequestCache = &requestCache{
	order:         list.New(),
	items:         make(map[[32]byte]*list.Element, requestCacheSize),
	maxBytes:      requestCacheMaxBytes,
	maxEntryBytes: requestCacheMaxEntryBytes,
	ttl:           requestCacheTTL,
}

func firstNonEmptyAnyString(raw any) string {
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case []byte:
		return strings.TrimSpace(string(v))
	default:
		return ""
	}
}

func appendResponseTextPart(parts *[]string, raw any) {
	text := firstNonEmptyAnyString(raw)
	if text != "" {
		*parts = append(*parts, text)
	}
}

func extractResponsesPromptText(body map[string]any) string {
	if len(body) == 0 {
		return ""
	}
	if prompt := firstNonEmptyAnyString(body["prompt"]); prompt != "" {
		return prompt
	}
	var parts []string
	extractResponsesInputText(body["input"], &parts)
	return strings.Join(parts, " ")
}

func extractResponsesInputText(raw any, parts *[]string) {
	switch v := raw.(type) {
	case string:
		appendResponseTextPart(parts, v)
	case []map[string]string:
		for _, item := range v {
			appendResponseTextPart(parts, item["content"])
		}
	case []map[string]any:
		for _, item := range v {
			extractResponsesMessageText(item, parts)
		}
	case []any:
		for _, item := range v {
			switch typed := item.(type) {
			case map[string]any:
				extractResponsesMessageText(typed, parts)
			case map[string]string:
				appendResponseTextPart(parts, typed["content"])
			case string:
				appendResponseTextPart(parts, typed)
			}
		}
	}
}

func extractResponsesMessageText(item map[string]any, parts *[]string) {
	if item == nil {
		return
	}
	appendResponseTextPart(parts, item["text"])
	content, ok := item["content"]
	if !ok {
		return
	}
	switch v := content.(type) {
	case string:
		appendResponseTextPart(parts, v)
	case []any:
		for _, rawPart := range v {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			appendResponseTextPart(parts, part["text"])
		}
	case []map[string]any:
		for _, part := range v {
			appendResponseTextPart(parts, part["text"])
		}
	}
}

func hasResponsesImageGenerationTool(body map[string]any) bool {
	rawTools, ok := body["tools"]
	if !ok || rawTools == nil {
		return false
	}
	tools, ok := rawTools.([]any)
	if !ok {
		return false
	}
	for _, rawTool := range tools {
		toolMap, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		if strings.TrimSpace(firstNonEmptyAnyString(toolMap["type"])) == "image_generation" {
			return true
		}
	}
	return false
}

// isImageGenNamespaceTool recognizes a namespace-form image generation tool declaration:
// { "type": "namespace", "name": "image_gen", ... }. Codex's /image skill uses
// this form to declare image generation, rather than the flat { "type": "image_generation" }.
func isImageGenNamespaceTool(tool map[string]any) bool {
	return strings.TrimSpace(firstNonEmptyAnyString(tool["type"])) == "namespace" &&
		strings.TrimSpace(firstNonEmptyAnyString(tool["name"])) == "image_gen"
}

// hasResponsesImageGenNamespaceTool detects whether the client brings its own namespace image generation declaration,
// covering two places: the top-level tools[], and the tool list embedded in type=additional_tools items of input[]
// (Responses Lite format). A client with such a declaration already has its own image generation path,
// so the hosted image_generation tool and the bridge instructions should not be added on top; the bridge text
// assumes the image_gen namespace is absent, so injecting it while the namespace is present has the opposite meaning.
func hasResponsesImageGenNamespaceTool(body map[string]any) bool {
	if tools, ok := body["tools"].([]any); ok {
		for _, rawTool := range tools {
			if toolMap, ok := rawTool.(map[string]any); ok && isImageGenNamespaceTool(toolMap) {
				return true
			}
		}
	}
	input, ok := body["input"].([]any)
	if !ok {
		return false
	}
	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		if strings.TrimSpace(firstNonEmptyAnyString(item["type"])) != "additional_tools" {
			continue
		}
		tools, ok := item["tools"].([]any)
		if !ok {
			continue
		}
		for _, rawTool := range tools {
			if toolMap, ok := rawTool.(map[string]any); ok && isImageGenNamespaceTool(toolMap) {
				return true
			}
		}
	}
	return false
}

func responsesImageGenerationToolChoice(body map[string]any) string {
	if len(body) == 0 {
		return ""
	}
	switch choice := body["tool_choice"].(type) {
	case string:
		return strings.TrimSpace(choice)
	case map[string]any:
		return strings.TrimSpace(firstNonEmptyAnyString(choice["type"]))
	default:
		return ""
	}
}

func hasResponsesImageGenerationToolChoice(body map[string]any) bool {
	return strings.EqualFold(responsesImageGenerationToolChoice(body), "image_generation")
}

func ensureResponsesImageGenerationTool(body map[string]any) bool {
	if len(body) == 0 {
		return false
	}
	defaultTool := map[string]any{
		"type":  "image_generation",
		"model": defaultImagesToolModel,
	}
	rawTools, ok := body["tools"]
	if !ok || rawTools == nil {
		body["tools"] = []any{defaultTool}
		return true
	}
	tools, ok := rawTools.([]any)
	if !ok {
		body["tools"] = []any{defaultTool}
		return true
	}
	for _, rawTool := range tools {
		toolMap, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		if strings.TrimSpace(firstNonEmptyAnyString(toolMap["type"])) == "image_generation" {
			return false
		}
	}
	if len(tools) >= maxTools {
		truncated := append([]any(nil), tools[:maxTools]...)
		truncated[maxTools-1] = defaultTool
		body["tools"] = truncated
		return true
	}
	body["tools"] = append(tools, defaultTool)
	return true
}

func firstResponsesImageGenerationTool(body map[string]any) map[string]any {
	rawTools, ok := body["tools"]
	if !ok || rawTools == nil {
		return nil
	}
	tools, ok := rawTools.([]any)
	if !ok {
		return nil
	}
	for _, rawTool := range tools {
		toolMap, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		if strings.TrimSpace(firstNonEmptyAnyString(toolMap["type"])) == "image_generation" {
			return toolMap
		}
	}
	return nil
}

func moveTopLevelResponsesImageOptions(body map[string]any) bool {
	toolMap := firstResponsesImageGenerationTool(body)
	if len(body) == 0 || toolMap == nil {
		return false
	}
	modified := false
	for _, key := range responsesImageGenerationOptionFields {
		value, exists := body[key]
		if !exists || !isResponsesImageGenerationOption(key, value) {
			continue
		}
		_, toolHas := toolMap[key]
		if key == "output_format" && strings.TrimSpace(firstNonEmptyAnyString(toolMap["format"])) != "" {
			toolHas = true
		}
		if key == "output_compression" {
			if _, hasAlias := toolMap["compression"]; hasAlias {
				toolHas = true
			}
		}
		if !toolHas {
			toolMap[key] = value
		}
		delete(body, key)
		modified = true
	}
	for _, key := range responsesImageGenerationUnsupportedOptionFields {
		if _, exists := body[key]; exists {
			delete(body, key)
			modified = true
		}
	}
	return modified
}

// codexWebSearchAllowedFields is the allowlist of web_search configuration fields accepted by the Codex upstream.
// Source of the measurement: probing chatgpt.com/backend-api/codex/responses directly with gpt-5.4-mini,
// these three fields are echoed back as-is and take effect; any field outside this set triggers
// 400 unknown_parameter.
var codexWebSearchAllowedFields = map[string]struct{}{
	"search_context_size": {},
	"user_location":       {},
	"filters":             {},
}

// normalizeResponsesWebSearchTools normalizes every web_search variant under the OpenAI Responses protocol
// variants (web_search_preview / web_search_preview_2025_03_11 /
// web_search_2025_08_26, etc.) to the only form the Codex upstream accepts: {"type":"web_search"}.
//
// The Codex backend only recognizes a bare "web_search", and returns for any other variant
// 400 {"detail":"Unsupported tool type: ..."}. The native OpenAI Responses
// endpoint supports these variants, so this function may only be called on the Codex upstream path.
//
// Normalization keeps the configuration fields Codex is known to accept (search_context_size / user_location /
// filters); all other unknown fields are dropped, to avoid triggering the upstream unknown_parameter validation.
func normalizeResponsesWebSearchTools(body map[string]any) bool {
	rawTools, ok := body["tools"]
	if !ok || rawTools == nil {
		return false
	}
	tools, ok := rawTools.([]any)
	if !ok {
		return false
	}
	modified := false
	for i, rawTool := range tools {
		toolMap, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		toolType := strings.TrimSpace(firstNonEmptyAnyString(toolMap["type"]))
		if toolType == "" || !strings.HasPrefix(toolType, "web_search") {
			continue
		}
		normalized := normalizeCodexWebSearchTool(toolMap)
		if mapsEqual(toolMap, normalized) {
			continue
		}
		tools[i] = normalized
		modified = true
	}
	if modified {
		body["tools"] = tools
	}
	return modified
}

// normalizeCodexWebSearchTool returns a new map containing only {type, <allowlisted fields>}.
// Before calling, make sure toolMap.type starts with "web_search".
func normalizeCodexWebSearchTool(toolMap map[string]any) map[string]any {
	out := map[string]any{"type": "web_search"}
	for k, v := range toolMap {
		if _, ok := codexWebSearchAllowedFields[k]; ok {
			out[k] = v
		}
	}
	return out
}

func mapsEqual(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		vb, ok := b[k]
		if !ok {
			return false
		}
		if !reflect.DeepEqual(va, vb) {
			return false
		}
	}
	return true
}

func normalizeResponsesImageGenerationTools(body map[string]any, promptText string) bool {
	rawTools, ok := body["tools"]
	if !ok || rawTools == nil {
		return false
	}
	tools, ok := rawTools.([]any)
	if !ok {
		return false
	}
	modified := false
	for _, rawTool := range tools {
		toolMap, ok := rawTool.(map[string]any)
		if !ok || strings.TrimSpace(firstNonEmptyAnyString(toolMap["type"])) != "image_generation" {
			continue
		}
		rawModel := strings.TrimSpace(firstNonEmptyAnyString(toolMap["model"]))
		toolModel, defaultSize := normalizeImageToolModelForPrompt(rawModel, promptText)
		if rawModel != toolModel {
			toolMap["model"] = toolModel
			modified = true
		}
		sizeValue, hasSize := toolMap["size"]
		sizeString, sizeIsString := sizeValue.(string)
		if defaultSize != "" && (!hasSize || (sizeIsString && strings.TrimSpace(sizeString) == "")) {
			toolMap["size"] = defaultSize
			modified = true
		}
		if _, ok := toolMap["output_format"]; !ok {
			if value := strings.TrimSpace(firstNonEmptyAnyString(toolMap["format"])); value != "" {
				toolMap["output_format"] = value
			} else {
				toolMap["output_format"] = "png"
			}
			modified = true
		}
		if _, ok := toolMap["output_compression"]; !ok {
			if value, exists := toolMap["compression"]; exists && value != nil {
				toolMap["output_compression"] = value
				modified = true
			}
		}
		if _, ok := toolMap["format"]; ok {
			delete(toolMap, "format")
			modified = true
		}
		if _, ok := toolMap["compression"]; ok {
			delete(toolMap, "compression")
			modified = true
		}
		for _, key := range responsesImageGenerationUnsupportedOptionFields {
			if _, ok := toolMap[key]; ok {
				delete(toolMap, key)
				modified = true
			}
		}
	}
	return modified
}

func normalizeResponsesPromptCompat(body map[string]any) bool {
	rawPrompt, hasPrompt := body["prompt"]
	if len(body) == 0 || !hasPrompt {
		return false
	}
	if _, hasInput := body["input"]; !hasInput {
		if prompt := strings.TrimSpace(firstNonEmptyAnyString(rawPrompt)); prompt != "" {
			body["input"] = prompt
		}
	}
	delete(body, "prompt")
	return true
}

func applyResponsesImageGenerationBridgeInstructions(body map[string]any) bool {
	if len(body) == 0 || !hasResponsesImageGenerationTool(body) {
		return false
	}
	existing, _ := body["instructions"].(string)
	if strings.Contains(existing, codexImageGenerationBridgeMarker) {
		return false
	}
	existing = strings.TrimRight(existing, " \t\r\n")
	if strings.TrimSpace(existing) == "" {
		body["instructions"] = codexImageGenerationBridgeText
		return true
	}
	body["instructions"] = existing + "\n\n" + codexImageGenerationBridgeText
	return true
}

// removeCodexImageGenerationBridgeText removes the image generation bridge marker block from instructions,
// the inverse of applyResponsesImageGenerationBridgeInstructions. Used when stripping the bridge text that was appended
// together with the injected tool; if the marker block is not present, the input is returned unchanged.
func removeCodexImageGenerationBridgeText(instructions string) string {
	const bridgeEndTag = "</codex2api-codex-image-generation>"
	for {
		start := strings.Index(instructions, codexImageGenerationBridgeMarker)
		if start < 0 {
			return instructions
		}
		rest := instructions[start:]
		end := strings.Index(rest, bridgeEndTag)
		head := strings.TrimRight(instructions[:start], " \t\r\n")
		if end < 0 {
			return head
		}
		tail := strings.TrimLeft(rest[end+len(bridgeEndTag):], " \t\r\n")
		switch {
		case head == "":
			instructions = tail
		case tail == "":
			instructions = head
		default:
			instructions = head + "\n\n" + tail
		}
	}
}

func hasTopLevelResponsesImageOptions(body map[string]any) bool {
	if len(body) == 0 {
		return false
	}
	for _, key := range responsesImageGenerationOptionFields {
		if value, exists := body[key]; exists && isResponsesImageGenerationOption(key, value) {
			return true
		}
	}
	return false
}

func isResponsesImageGenerationOption(key string, value any) bool {
	if value == nil {
		return false
	}
	if key == "background" {
		// Responses uses a boolean for asynchronous execution; image generation
		// uses a string (auto/opaque/transparent). Never move that boolean into
		// an image tool or use it to infer an image-generation request.
		_, imageBackground := value.(string)
		return imageBackground
	}
	return true
}

func isStructuredResponsesFormatType(formatType string) bool {
	switch strings.ToLower(strings.TrimSpace(formatType)) {
	case "json_schema", "json_object":
		return true
	default:
		return false
	}
}

func hasStructuredResponsesFormat(body map[string]any) bool {
	if len(body) == 0 {
		return false
	}
	if text, ok := body["text"].(map[string]any); ok {
		if format, ok := text["format"].(map[string]any); ok {
			if isStructuredResponsesFormatType(firstNonEmptyAnyString(format["type"])) {
				return true
			}
		}
	}
	if responseFormat, ok := body["response_format"].(map[string]any); ok {
		return isStructuredResponsesFormatType(firstNonEmptyAnyString(responseFormat["type"]))
	}
	return false
}

// responsesModelRejectsHostedImageTool reports whether the model does not support the hosted image_generation
// tool. gpt-5.3-codex-spark is a text-only Codex model under ChatGPT accounts, and the upstream rejects outright
// requests that carry a hosted image tool (issue #230).
func responsesModelRejectsHostedImageTool(body map[string]any) bool {
	model := strings.TrimSpace(firstNonEmptyAnyString(body["model"]))
	return strings.EqualFold(model, proOnlySparkModel)
}

func shouldAutoInjectResponsesImageGenerationTool(body map[string]any) bool {
	if len(body) == 0 || hasResponsesImageGenerationTool(body) {
		return false
	}
	// Not added on top when the client brings its own namespace image generation declaration (see hasResponsesImageGenNamespaceTool).
	if hasResponsesImageGenNamespaceTool(body) {
		return false
	}
	// Does not automatically inject the default image tool and bridge instructions for models that reject hosted image tools;
	// an image tool the user explicitly brings is still kept by the hasResponsesImageGenerationTool branch above.
	if responsesModelRejectsHostedImageTool(body) {
		return false
	}
	if hasResponsesImageGenerationToolChoice(body) {
		return true
	}
	if hasTopLevelResponsesImageOptions(body) {
		return true
	}
	// An implicit image tool must not evict an explicitly supplied tool at capacity.
	if tools, ok := body["tools"].([]any); ok && len(tools) >= maxTools {
		return false
	}
	return !hasStructuredResponsesFormat(body)
}

func normalizeResponsesImageOnlyModel(body map[string]any) bool {
	if len(body) == 0 {
		return false
	}
	imageModel := strings.TrimSpace(firstNonEmptyAnyString(body["model"]))
	if !isImageOnlyModel(imageModel) {
		return false
	}

	modified := false
	tools, _ := body["tools"].([]any)
	imageToolIndex := -1
	for i, rawTool := range tools {
		toolMap, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		if strings.TrimSpace(firstNonEmptyAnyString(toolMap["type"])) == "image_generation" {
			imageToolIndex = i
			break
		}
	}
	if imageToolIndex < 0 {
		tools = append(tools, map[string]any{
			"type":  "image_generation",
			"model": imageModel,
		})
		imageToolIndex = len(tools) - 1
		body["tools"] = tools
		modified = true
	}

	if toolMap, ok := tools[imageToolIndex].(map[string]any); ok {
		if strings.TrimSpace(firstNonEmptyAnyString(toolMap["model"])) == "" {
			toolMap["model"] = imageModel
			modified = true
		}
	}

	if _, ok := body["tool_choice"]; !ok {
		body["tool_choice"] = map[string]any{"type": "image_generation"}
		modified = true
	}
	mainModel := imagesMainModel()
	if imageModel != mainModel {
		modified = true
	}
	body["model"] = mainModel
	return modified
}

// normalizeResponsesCompactionItems converts {"type":"compaction","summary":"..."}
// items in body["input"] into developer-role messages so the upstream Codex
// /responses endpoint accepts them. Items with empty or missing summary text
// are dropped. Codex CLI compresses prior turns into compaction items expecting
// them to be forwarded as conversation context; the upstream rejects the type
// with "Invalid input type 'compaction' at index N", so we translate in place.
//
// Opaque Compact v2 items are left untouched so they remain source-affine.
// Known reversible emulated envelopes are decoded into the same developer
// summary representation as plaintext compaction items.
func normalizeResponsesCompactionItems(body map[string]any) bool {
	if len(body) == 0 {
		return false
	}
	inputItems, ok := body["input"].([]any)
	if !ok {
		return false
	}

	modified := false
	out := make([]any, 0, len(inputItems))
	for _, raw := range inputItems {
		itemMap, ok := raw.(map[string]any)
		if !ok {
			out = append(out, raw)
			continue
		}
		itemType := firstNonEmptyAnyString(itemMap["type"])
		if !isResponsesCompactionItemType(itemType) {
			out = append(out, raw)
			continue
		}

		if encryptedContent := firstNonEmptyAnyString(itemMap["encrypted_content"]); encryptedContent != "" {
			rawEncryptedContent, _ := itemMap["encrypted_content"].(string)
			if summaryText, portable := decodePortableCompactionSummary(rawEncryptedContent); portable {
				out = append(out, responsesCompactionDeveloperMessage(summaryText))
				modified = true
				continue
			}
			// Unknown encrypted state must be forwarded verbatim.
			out = append(out, raw)
			continue
		}
		if itemType != "compaction" {
			out = append(out, raw)
			continue
		}

		summaryText := compactionSummaryText(itemMap["summary"])
		if summaryText == "" {
			summaryText = compactionSummaryText(itemMap["text"])
		}
		if summaryText == "" {
			modified = true
			continue
		}

		out = append(out, responsesCompactionDeveloperMessage(summaryText))
		modified = true
	}

	if modified {
		body["input"] = out
	}
	return modified
}

// normalizeResponsesSystemRoleMessages rewrites message items with role=system in input[] to
// the developer role. The upstream Codex /responses does not accept the system role (it reports "System messages
// are not allowed"), while the chat and Anthropic translation paths both already map system to
// developer (buildInput / buildCodexInput); the native Responses pass-through path fills in
// the same semantics here (issue #409). The content is kept as-is, and content part types are left to
// normalizeResponsesContentPartTypes, which normalizes them by role.
func normalizeResponsesSystemRoleMessages(body map[string]any) bool {
	if len(body) == 0 {
		return false
	}
	inputItems, ok := body["input"].([]any)
	if !ok {
		return false
	}

	modified := false
	for _, raw := range inputItems {
		itemMap, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if !isResponsesMessageInputItem(itemMap) {
			continue
		}
		if strings.TrimSpace(firstNonEmptyAnyString(itemMap["role"])) != "system" {
			continue
		}
		itemMap["role"] = "developer"
		modified = true
	}
	return modified
}

// normalizeResponsesToolCallArgumentTypes fixes the JSON type of the arguments of tool call items in input[].
// The upstream requires different types for different item types: function_call.arguments must be a
// string (JSON-encoded), while tool_search_call.arguments must be an object. Clients and cache
// replay usually feeds the previous turn's output items back unchanged, and the upstream rejects a type mismatch with a 400:
// "Invalid type for 'input[N].arguments': expected an object, but got a string
// instead." (issue #330).
func normalizeResponsesToolCallArgumentTypes(body map[string]any) bool {
	inputItems, ok := body["input"].([]any)
	if !ok {
		return false
	}

	modified := false
	for _, raw := range inputItems {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		args, hasArgs := item["arguments"]
		if !hasArgs {
			continue
		}
		switch firstNonEmptyAnyString(item["type"]) {
		case "function_call":
			if _, isString := args.(string); isString {
				continue
			}
			if encoded, err := json.Marshal(args); err == nil {
				item["arguments"] = string(encoded)
				modified = true
			}
		case "tool_search_call":
			s, isString := args.(string)
			if !isString {
				continue
			}
			var obj map[string]any
			if strings.TrimSpace(s) == "" {
				obj = map[string]any{}
			} else if err := json.Unmarshal([]byte(s), &obj); err != nil || obj == nil {
				continue
			}
			item["arguments"] = obj
			modified = true
		}
	}
	return modified
}

// normalizeOrdinaryFunctionCallArguments validates the JSON string required by
// an ordinary Responses function_call. Empty arguments are a widely emitted
// shorthand for an empty object; all other malformed values are rejected so a
// truncated call cannot be persisted and replayed into later turns.
func normalizeOrdinaryFunctionCallArguments(arguments string) (string, bool) {
	if strings.TrimSpace(arguments) == "" {
		return "{}", true
	}
	if !json.Valid([]byte(arguments)) {
		return "", false
	}
	return arguments, true
}

// sanitizeMalformedResponsesFunctionCalls removes malformed ordinary
// function_call history and its matching function_call_output. Provider-native
// and custom tool items deliberately remain untouched: their arguments/input
// may be free-form text or another provider-specific shape rather than JSON.
func sanitizeMalformedResponsesFunctionCalls(body map[string]any) bool {
	inputItems, ok := body["input"].([]any)
	if !ok || len(inputItems) == 0 {
		return false
	}

	invalidCallIDs := make(map[string]struct{})
	validCallIDs := make(map[string]struct{})
	invalidEmptyCalls := 0
	modified := false
	withoutInvalidCalls := make([]any, 0, len(inputItems))

	for _, raw := range inputItems {
		item, ok := raw.(map[string]any)
		if !ok || strings.TrimSpace(firstNonEmptyAnyString(item["type"])) != "function_call" {
			withoutInvalidCalls = append(withoutInvalidCalls, raw)
			continue
		}

		arguments, isString := item["arguments"].(string)
		if !isString {
			// normalizeResponsesToolCallArgumentTypes runs immediately before this
			// sanitizer. If marshaling failed there, fail closed here.
			callID := strings.TrimSpace(firstNonEmptyAnyString(item["call_id"]))
			if callID == "" {
				invalidEmptyCalls++
			} else {
				invalidCallIDs[callID] = struct{}{}
			}
			modified = true
			continue
		}
		normalized, valid := normalizeOrdinaryFunctionCallArguments(arguments)
		callID := strings.TrimSpace(firstNonEmptyAnyString(item["call_id"]))
		if !valid {
			if callID == "" {
				invalidEmptyCalls++
			} else {
				invalidCallIDs[callID] = struct{}{}
			}
			modified = true
			continue
		}
		if normalized != arguments {
			item["arguments"] = normalized
			modified = true
		}
		if callID != "" {
			validCallIDs[callID] = struct{}{}
		}
		withoutInvalidCalls = append(withoutInvalidCalls, raw)
	}

	for callID := range validCallIDs {
		delete(invalidCallIDs, callID)
	}
	if len(invalidCallIDs) == 0 && invalidEmptyCalls == 0 {
		if modified {
			body["input"] = withoutInvalidCalls
		}
		return modified
	}

	filtered := make([]any, 0, len(withoutInvalidCalls))
	for _, raw := range withoutInvalidCalls {
		item, ok := raw.(map[string]any)
		if !ok || strings.TrimSpace(firstNonEmptyAnyString(item["type"])) != "function_call_output" {
			filtered = append(filtered, raw)
			continue
		}
		callID := strings.TrimSpace(firstNonEmptyAnyString(item["call_id"]))
		if callID == "" && invalidEmptyCalls > 0 {
			invalidEmptyCalls--
			modified = true
			continue
		}
		if _, invalid := invalidCallIDs[callID]; invalid {
			modified = true
			continue
		}
		filtered = append(filtered, raw)
	}
	body["input"] = filtered
	return modified
}

// repairResponsesToolCallPairing repairs the call_id pairing between tool call items and output items in input[].
// Some clients (such as VSCode Copilot's direct Responses connection) trim or summarize context in long sessions,
// and drop half of a pair: with only *_call_output left, the upstream returns 400 "No tool call found for
// function call output with call_id ..." (issue #414); with only *_call left, the upstream returns 400
// "No tool output found for function call ...". Repair strategy:
//   - orphan *_call_output (including those missing a call_id): rewrite as a user message that keeps the output text,
//     so context is not lost by dropping it outright;
//   - orphan function_call / custom_tool_call: append a placeholder output right after it;
//     other *_call types have an unclear shape, so they are kept as-is and not synthesized.
func repairResponsesToolCallPairing(body map[string]any) bool {
	inputItems, ok := body["input"].([]any)
	if !ok || len(inputItems) == 0 {
		return false
	}

	callIDs := make(map[string]bool)
	outputIDs := make(map[string]bool)
	for _, raw := range inputItems {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		callID := strings.TrimSpace(firstNonEmptyAnyString(item["call_id"]))
		if callID == "" {
			continue
		}
		typ := strings.TrimSpace(firstNonEmptyAnyString(item["type"]))
		switch {
		case isCodexToolCallContextType(typ):
			callIDs[callID] = true
		case isCodexToolCallOutputType(typ):
			outputIDs[callID] = true
		}
	}

	orphanOutputs, orphanCalls := 0, 0
	out := make([]any, 0, len(inputItems))
	for _, raw := range inputItems {
		item, ok := raw.(map[string]any)
		if !ok {
			out = append(out, raw)
			continue
		}
		typ := strings.TrimSpace(firstNonEmptyAnyString(item["type"]))
		callID := strings.TrimSpace(firstNonEmptyAnyString(item["call_id"]))
		switch {
		case isCodexToolCallOutputType(typ):
			if callID != "" && callIDs[callID] {
				out = append(out, raw)
				continue
			}
			out = append(out, orphanToolOutputAsMessage(callID, item["output"]))
			orphanOutputs++
		case isCodexToolCallContextType(typ):
			out = append(out, raw)
			if callID == "" || outputIDs[callID] {
				continue
			}
			outputType := codexToolCallOutputTypeForCall(typ)
			if outputType == "" {
				continue
			}
			out = append(out, map[string]any{
				"type":    outputType,
				"call_id": callID,
				"output":  "[tool output was not recorded]",
			})
			// Mark as already filled, so the same call_id is not synthesized again when it appears again.
			outputIDs[callID] = true
			orphanCalls++
		default:
			out = append(out, raw)
		}
	}

	if orphanOutputs == 0 && orphanCalls == 0 {
		return false
	}
	body["input"] = out
	log.Printf("repaired input tool call pairing: orphan outputs converted to messages %d, orphan calls given placeholder outputs %d", orphanOutputs, orphanCalls)
	return true
}

// orphanToolOutputAsMessage rewrites unpairable tool output items as user messages,
// keeping the output text for the model to keep referring to.
func orphanToolOutputAsMessage(callID string, output any) map[string]any {
	label := "[Tool output from an earlier turn]"
	if callID != "" {
		label = "[Tool output from an earlier turn, call_id " + callID + "]"
	}
	return map[string]any{
		"type": "message",
		"role": "user",
		"content": []any{
			map[string]any{
				"type": "input_text",
				"text": label + "\n" + flattenToolOutputText(output),
			},
		},
	}
}

// flattenToolOutputText flattens the output field of *_call_output into plain text.
// output may be a string, or a content array in the form [{type:"output_text",text:"..."}].
func flattenToolOutputText(output any) string {
	switch v := output.(type) {
	case nil:
		return ""
	case string:
		return v
	case []any:
		var sb strings.Builder
		for _, raw := range v {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if text := firstNonEmptyAnyString(part["text"]); text != "" {
				if sb.Len() > 0 {
					sb.WriteString("\n")
				}
				sb.WriteString(text)
			}
		}
		if sb.Len() > 0 {
			return sb.String()
		}
	}
	if encoded, err := json.Marshal(output); err == nil {
		return string(encoded)
	}
	return ""
}

// codexToolCallOutputTypeForCall returns the output item type that can safely be synthesized as a placeholder for a call item.
func codexToolCallOutputTypeForCall(callType string) string {
	switch callType {
	case "function_call":
		return "function_call_output"
	case "custom_tool_call":
		return "custom_tool_call_output"
	default:
		return ""
	}
}

func normalizeResponsesInputMessageContent(body map[string]any) bool {
	inputItems, ok := body["input"].([]any)
	if !ok {
		return false
	}

	modified := false
	for _, raw := range inputItems {
		itemMap, ok := raw.(map[string]any)
		if !ok || !isResponsesMessageInputItem(itemMap) {
			continue
		}
		if content, exists := itemMap["content"]; !exists || content == nil {
			itemMap["content"] = ""
			modified = true
		}
	}
	return modified
}

func isResponsesMessageInputItem(item map[string]any) bool {
	itemType := strings.TrimSpace(firstNonEmptyAnyString(item["type"]))
	if itemType == "message" {
		return true
	}
	if itemType != "" {
		return false
	}

	switch strings.TrimSpace(firstNonEmptyAnyString(item["role"])) {
	case "user", "assistant", "developer", "system":
		return true
	default:
		return false
	}
}

func normalizeResponsesInputItemIDs(body map[string]any) bool {
	inputItems, ok := body["input"].([]any)
	if !ok {
		return false
	}

	modified := false
	for _, raw := range inputItems {
		itemMap, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if _, exists := itemMap["id"]; exists {
			delete(itemMap, "id")
			modified = true
		}
	}
	return modified
}

// responsesInputInternalMetadataField is internal metadata that Codex CLI attaches to the top level of input items when a custom provider is named "OpenAI".
// The ChatGPT backend does not accept this field and rejects the request outright with a 400.
const responsesInputInternalMetadataField = "internal_chat_message_metadata_passthrough"

// stripResponsesInputInternalMetadata deletes only the internal metadata fields on top-level input[] items,
// and leaves user content with the same name in content/arguments untouched.
func stripResponsesInputInternalMetadata(body map[string]any) bool {
	inputItems, ok := body["input"].([]any)
	if !ok {
		return false
	}

	modified := false
	for _, raw := range inputItems {
		itemMap, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if _, exists := itemMap[responsesInputInternalMetadataField]; exists {
			delete(itemMap, responsesInputInternalMetadataField)
			modified = true
		}
	}
	return modified
}

func normalizeResponsesContentPartTypes(body map[string]any) bool {
	inputItems, ok := body["input"].([]any)
	if !ok {
		return false
	}

	modified := false
	for _, raw := range inputItems {
		itemMap, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role := strings.TrimSpace(firstNonEmptyAnyString(itemMap["role"]))
		if normalizeResponsesContentItemType(itemMap, role) {
			modified = true
		}
		contentItems, ok := itemMap["content"].([]any)
		if !ok {
			continue
		}
		for _, rawContent := range contentItems {
			contentMap, ok := rawContent.(map[string]any)
			if !ok {
				continue
			}
			if normalizeResponsesContentItemType(contentMap, role) {
				modified = true
			}
		}
	}
	return modified
}

func normalizeResponsesContentItemType(item map[string]any, role string) bool {
	itemType := strings.TrimSpace(firstNonEmptyAnyString(item["type"]))
	modified := false

	switch itemType {
	case "file":
		item["type"] = "input_file"
		itemType = "input_file"
		modified = true
	case "image", "image_url":
		item["type"] = "input_image"
		itemType = "input_image"
		modified = true
	case "text":
		if strings.TrimSpace(role) == "assistant" {
			item["type"] = "output_text"
		} else {
			item["type"] = "input_text"
		}
		itemType = firstNonEmptyAnyString(item["type"])
		modified = true
	case "input_text":
		if strings.TrimSpace(role) == "assistant" {
			item["type"] = "output_text"
			itemType = "output_text"
			modified = true
		}
	case "output_text":
		if strings.TrimSpace(role) != "assistant" {
			item["type"] = "input_text"
			itemType = "input_text"
			modified = true
		}
	}

	if itemType == "input_file" {
		if normalizeResponsesInputFileFields(item) {
			modified = true
		}
	}
	if itemType == "input_image" || itemType == "computer_screenshot" {
		if normalizeResponsesImageURLField(item) {
			modified = true
		}
	}
	return modified
}

func isInvalidEncryptedContentError(statusCode int, body []byte) bool {
	if statusCode != http.StatusBadRequest {
		return false
	}
	if isMissingEncryptedContentError(body) {
		return true
	}
	for _, path := range []string{"error.code", "detail.code", "code"} {
		if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(body, path).String()), "invalid_encrypted_content") {
			return true
		}
	}
	msgParts := []string{
		gjson.GetBytes(body, "error.message").String(),
		gjson.GetBytes(body, "detail").String(),
		string(body),
	}
	for _, msg := range msgParts {
		msg = strings.ToLower(msg)
		if strings.Contains(msg, "invalid_encrypted_content") {
			return true
		}
		if strings.Contains(msg, "encrypted content") &&
			(strings.Contains(msg, "could not be verified") || strings.Contains(msg, "could not be decrypted")) {
			return true
		}
	}
	return false
}

func isMissingEncryptedContentError(body []byte) bool {
	code := strings.TrimSpace(gjson.GetBytes(body, "error.code").String())
	param := strings.TrimSpace(gjson.GetBytes(body, "error.param").String())
	if !strings.EqualFold(code, "missing_required_parameter") || !strings.HasSuffix(param, ".encrypted_content") {
		return false
	}
	msg := strings.ToLower(gjson.GetBytes(body, "error.message").String())
	return strings.Contains(msg, "encrypted_content")
}

func stripInvalidEncryptedContentFromResponsesBody(body []byte) ([]byte, bool) {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil || root == nil {
		return body, false
	}
	input, ok := root["input"]
	if !ok {
		return body, false
	}
	strippedInput, changed, keep := stripInvalidEncryptedContentValue(input, false)
	if !changed {
		return body, false
	}
	if keep {
		root["input"] = strippedInput
	} else {
		delete(root, "input")
	}
	stripped, err := json.Marshal(root)
	if err != nil {
		return body, false
	}
	return stripped, true
}

// isEncryptedCompactionItemType reports whether this input item type is a compaction item whose ciphertext is required.
// Same scope as gjsonResultIsCompactionHistory, and also includes compaction_summary fed back from the response side.
func isEncryptedCompactionItemType(itemType string) bool {
	switch strings.ToLower(strings.TrimSpace(itemType)) {
	case "compaction", "context_compaction", "compaction_summary":
		return true
	default:
		return false
	}
}

func stripInvalidEncryptedContentValue(value any, arrayItem bool) (any, bool, bool) {
	switch v := value.(type) {
	case []any:
		changed := false
		out := make([]any, 0, len(v))
		for _, item := range v {
			stripped, itemChanged, keep := stripInvalidEncryptedContentValue(item, true)
			if itemChanged {
				changed = true
			}
			if !keep {
				changed = true
				continue
			}
			out = append(out, stripped)
		}
		return out, changed, true
	case map[string]any:
		changed := false
		itemType := strings.TrimSpace(firstNonEmptyAnyString(v["type"]))
		_, hasEncrypted := v["encrypted_content"]
		switch {
		case itemType == "reasoning":
			if arrayItem {
				return nil, true, false
			}
			if hasEncrypted {
				delete(v, "encrypted_content")
			}
			if len(v) == 1 {
				return nil, true, false
			}
			changed = true
		case hasEncrypted && isEncryptedCompactionItemType(itemType):
			// The encrypted_content of a compaction item is required: stripping only the field would leave
			// an empty {"type":"compaction"} shell to go upstream, and the upstream would
			// reject it again with missing_required_parameter, while the single retry gate has already been used up.
			// When ciphertext is present, drop the whole item, symmetric with the reasoning branch above; a compaction item without ciphertext
			// has no account binding anyway, so it is kept as-is.
			return nil, true, false
		case hasEncrypted:
			delete(v, "encrypted_content")
			changed = true
		}
		for key, child := range v {
			stripped, childChanged, keep := stripInvalidEncryptedContentValue(child, false)
			if childChanged {
				changed = true
			}
			if keep {
				v[key] = stripped
			} else {
				delete(v, key)
			}
		}
		return v, changed, true
	default:
		return value, false, true
	}
}

func responsesInputRaw(body []byte) string {
	input := gjson.GetBytes(body, "input")
	if !input.Exists() {
		return ""
	}
	return input.Raw
}

func dropBareReasoningInputItems(body map[string]any) bool {
	input, ok := body["input"]
	if !ok {
		return false
	}
	cleaned, changed, keep := dropBareReasoningInputValue(input)
	if !changed {
		return false
	}
	if keep {
		body["input"] = cleaned
	} else {
		delete(body, "input")
	}
	return true
}

// codexEncryptedContentPrefix 是 Codex 上游 reasoning.encrypted_content 的
// Fernet 令牌前缀：版本字节 0x80 使 base64 编码恒以 "gAAAA" 开头，可据此区分血统。
const codexEncryptedContentPrefix = "gAAAA"

func dropBareReasoningInputValue(value any) (any, bool, bool) {
	switch v := value.(type) {
	case []any:
		changed := false
		out := make([]any, 0, len(v))
		for _, item := range v {
			cleaned, itemChanged, keep := dropBareReasoningInputValue(item)
			if itemChanged {
				changed = true
			}
			if !keep {
				changed = true
				continue
			}
			out = append(out, cleaned)
		}
		return out, changed, true
	case map[string]any:
		if strings.TrimSpace(firstNonEmptyAnyString(v["type"])) == "reasoning" {
			// A reasoning item with no ciphertext, or with ciphertext from another channel (no Fernet prefix), is dropped as a whole:
			// the Codex upstream requires reasoning to always carry its own decryptable encrypted_content;
			// if it is missing, the upstream reports missing_required_parameter, and foreign ciphertext reports
			// invalid_encrypted_content (issue #565).
			ec := firstNonEmptyAnyString(v["encrypted_content"])
			if ec == "" || !strings.HasPrefix(ec, codexEncryptedContentPrefix) {
				return nil, true, false
			}
			// The Codex reasoning schema does not recognize the status field (400 unknown_parameter),
			// and its own output never carries it either; in cross-channel sessions the client may feed back
			// external-channel reasoning output that carries status as-is (issue #565).
			changed := false
			// Responses accepts encrypted reasoning and summary; display-only
			// content is not accepted when replayed as input.
			for _, field := range []string{"status", "content"} {
				if _, has := v[field]; has {
					delete(v, field)
					changed = true
				}
			}
			return v, changed, true
		}
		return v, false, true
	default:
		return value, false, true
	}
}

func normalizeResponsesInputFileFields(item map[string]any) bool {
	rawFile, hasFile := item["file"]
	if !hasFile {
		return false
	}

	if fileMap, ok := rawFile.(map[string]any); ok {
		for _, key := range []string{"file_id", "file_data", "file_url", "filename"} {
			if _, exists := item[key]; exists {
				continue
			}
			if value, exists := fileMap[key]; exists && value != nil {
				item[key] = value
			}
		}
	} else if fileID := strings.TrimSpace(firstNonEmptyAnyString(rawFile)); fileID != "" {
		if _, exists := item["file_id"]; !exists {
			item["file_id"] = fileID
		}
	}
	delete(item, "file")
	return true
}

func normalizeResponsesImageURLField(item map[string]any) bool {
	rawImageURL, ok := item["image_url"]
	if !ok {
		return false
	}
	imageURLMap, ok := rawImageURL.(map[string]any)
	if !ok {
		return false
	}
	if url := strings.TrimSpace(firstNonEmptyAnyString(imageURLMap["url"])); url != "" {
		item["image_url"] = url
		return true
	}
	return false
}

// compactionSummaryText extracts a usable summary string from a compaction
// item's summary field. Strings pass through trimmed; non-string values are
// JSON-serialized so the model still receives the original payload as text.
func compactionSummaryText(raw any) string {
	if raw == nil {
		return ""
	}
	if s, ok := raw.(string); ok {
		return strings.TrimSpace(s)
	}
	if b, err := json.Marshal(raw); err == nil {
		return strings.TrimSpace(string(b))
	}
	return ""
}

func truncateToolsPreservingImageGeneration(tools []any) []any {
	if len(tools) <= maxTools {
		return tools
	}
	imageIndex := -1
	for i, rawTool := range tools {
		toolMap, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		if strings.TrimSpace(firstNonEmptyAnyString(toolMap["type"])) == "image_generation" {
			imageIndex = i
			break
		}
	}
	if imageIndex < 0 || imageIndex < maxTools {
		return tools[:maxTools]
	}
	truncated := append([]any(nil), tools[:maxTools]...)
	truncated[maxTools-1] = tools[imageIndex]
	return truncated
}

func (c *requestCache) ttlValue() time.Duration {
	if c.ttl > 0 {
		return c.ttl
	}
	return requestCacheTTL
}

func (c *requestCache) maxBytesValue() int {
	if c.maxBytes > 0 {
		return c.maxBytes
	}
	return requestCacheMaxBytes
}

func (c *requestCache) maxEntryBytesValue() int {
	if c.maxEntryBytes > 0 {
		return c.maxEntryBytes
	}
	return requestCacheMaxEntryBytes
}

func (c *requestCache) removeElementLocked(elem *list.Element) {
	entry := elem.Value.(*requestCacheEntry)
	c.order.Remove(elem)
	delete(c.items, entry.key)
	c.bytes -= entry.size
	if c.bytes < 0 {
		c.bytes = 0
	}
}

func (c *requestCache) pruneExpiredLocked(now time.Time) {
	for _, elem := range c.items {
		entry := elem.Value.(*requestCacheEntry)
		if !entry.expiresAt.IsZero() && now.After(entry.expiresAt) {
			c.removeElementLocked(elem)
		}
	}
}

func (c *requestCache) get(key [32]byte) (openAIRequest, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	elem, ok := c.items[key]
	if !ok {
		return openAIRequest{}, false
	}
	entry := elem.Value.(*requestCacheEntry)
	if !entry.expiresAt.IsZero() && time.Now().After(entry.expiresAt) {
		c.removeElementLocked(elem)
		return openAIRequest{}, false
	}
	c.order.MoveToFront(elem)
	return entry.req, true
}

func (c *requestCache) put(key [32]byte, req openAIRequest, size int) {
	if size <= 0 || size > c.maxEntryBytesValue() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	c.pruneExpiredLocked(now)
	expiresAt := now.Add(c.ttlValue())
	if elem, ok := c.items[key]; ok {
		entry := elem.Value.(*requestCacheEntry)
		c.bytes -= entry.size
		entry.req = req
		entry.size = size
		entry.expiresAt = expiresAt
		c.bytes += size
		c.order.MoveToFront(elem)
	} else {
		elem := c.order.PushFront(&requestCacheEntry{key: key, req: req, size: size, expiresAt: expiresAt})
		c.items[key] = elem
		c.bytes += size
	}
	for c.order.Len() > requestCacheSize || c.bytes > c.maxBytesValue() {
		tail := c.order.Back()
		if tail == nil {
			break
		}
		c.removeElementLocked(tail)
	}
}

// The estimated overhead of the parsed structure resident on the heap: besides the raw JSON, the slices themselves must also be counted.
// The JSON of 20000 short messages is only about 60KB, but the backing array of []openAIMessage takes ~1.5MB.
const (
	parsedRequestSizeCeiling = requestCacheMaxEntryBytes + 1
	parsedMessageOverhead    = 128 // openAIMessage struct + slice growth headroom
	parsedToolCallOverhead   = 128 // openAIToolCall struct + slice growth headroom
	parsedRawJSONOverhead    = 32  // json.RawMessage header
)

func estimateParsedRequestSize(rawJSON []byte, req *openAIRequest) int {
	size := int64(len(rawJSON)) * 3
	size += int64(cap(req.Messages)) * parsedMessageOverhead
	for i := range req.Messages {
		size += int64(cap(req.Messages[i].ToolCalls)) * parsedToolCallOverhead
	}
	size += int64(cap(req.Tools)) * parsedRawJSONOverhead
	if size > parsedRequestSizeCeiling {
		return parsedRequestSizeCeiling
	}
	return int(size)
}

// cachedOrParse gets the request from the cache or parses it, and returns the struct (Unmarshal at most once)
func cachedOrParse(rawJSON []byte) openAIRequest {
	if len(rawJSON) == 0 {
		return openAIRequest{}
	}
	key := sha256.Sum256(rawJSON)
	if req, ok := globalRequestCache.get(key); ok {
		return req
	}
	var req openAIRequest
	_ = json.Unmarshal(rawJSON, &req)
	// The parsed request retains more than the raw JSON (message slices,
	// RawMessage copies, tool structs). Charge a conservative multiplier plus
	// per-slice overhead so the byte budget bounds retained heap, not just wire
	// bytes.
	globalRequestCache.put(key, req, estimateParsedRequestSize(rawJSON, &req))
	return req
}

// ==================== request translation: OpenAI Chat Completions → Codex Responses ====================

// TranslateRequest converts an OpenAI Chat Completions request to Codex Responses format
// uses the Unmarshal → build map → Marshal pattern, serializing JSON only once
func TranslateRequest(rawJSON []byte) ([]byte, error) {
	parsed := cachedOrParse(rawJSON)
	req := sanitizeChatCompletionToolHistory(parsed)
	if err := validateChatCompletionFunctionNames(req); err != nil {
		return nil, err
	}
	out := buildChatResponsesRequest(req)
	if access := gjson.GetBytes(rawJSON, "access_programs"); access.Exists() {
		out["access_programs"] = json.RawMessage(access.Raw)
	}
	// The tool name sanitization mapping is derived from the parse result before history sanitization, and matches the response-side ChatToolNameRestoreMap
	// which uses the same input, so the dedup suffixes agree on both sides.
	applyCodexToolNameMap(out, buildCodexToolNameMap(collectChatToolNames(parsed)))
	return json.Marshal(out)
}

// sanitizeChatCompletionToolHistory removes truncated ordinary function calls
// together with their matching tool outputs before converting historical Chat
// messages to Responses input. Empty arguments are the common provider shorthand
// for an empty object and are normalized to {}. Custom/provider tool shapes are
// left untouched because their input is not required to be JSON.
func sanitizeChatCompletionToolHistory(req openAIRequest) openAIRequest {
	if len(req.Messages) == 0 {
		return req
	}

	invalidCallIDs := make(map[string]struct{})
	validCallIDs := make(map[string]struct{})
	invalidEmptyCalls := 0
	filtered := make([]openAIMessage, 0, len(req.Messages))
	modified := false

	for _, message := range req.Messages {
		if message.Role != "assistant" || len(message.ToolCalls) == 0 {
			filtered = append(filtered, message)
			continue
		}
		calls := make([]openAIToolCall, 0, len(message.ToolCalls))
		for _, toolCall := range message.ToolCalls {
			if toolCall.Type != "" && toolCall.Type != "function" {
				calls = append(calls, toolCall)
				continue
			}
			arguments := strings.TrimSpace(toolCall.Function.Arguments)
			if arguments == "" {
				toolCall.Function.Arguments = "{}"
				arguments = "{}"
				modified = true
			}
			if !json.Valid([]byte(arguments)) {
				modified = true
				if callID := strings.TrimSpace(toolCall.ID); callID != "" {
					invalidCallIDs[callID] = struct{}{}
				} else {
					invalidEmptyCalls++
				}
				continue
			}
			if callID := strings.TrimSpace(toolCall.ID); callID != "" {
				validCallIDs[callID] = struct{}{}
			}
			calls = append(calls, toolCall)
		}
		for callID := range validCallIDs {
			delete(invalidCallIDs, callID)
		}
		message.ToolCalls = calls
		if len(calls) == 0 && strings.TrimSpace(rawMessageToString(message.Content)) == "" {
			continue
		}
		filtered = append(filtered, message)
	}

	if len(invalidCallIDs) > 0 || invalidEmptyCalls > 0 {
		withoutOutputs := filtered[:0]
		for _, message := range filtered {
			if message.Role == "tool" {
				callID := strings.TrimSpace(message.ToolCallID)
				if callID == "" && invalidEmptyCalls > 0 {
					invalidEmptyCalls--
					modified = true
					continue
				}
				if _, invalid := invalidCallIDs[callID]; invalid {
					modified = true
					continue
				}
			}
			withoutOutputs = append(withoutOutputs, message)
		}
		filtered = withoutOutputs
	}
	if modified {
		req.Messages = filtered
	}
	return req
}

func buildChatResponsesRequest(req openAIRequest) map[string]any {
	// Build the output map (only contains the fields Codex needs)
	out := map[string]any{
		"model":   req.Model,
		"stream":  true,
		"store":   false,
		"include": []string{"reasoning.encrypted_content"},
	}

	// 1. messages → input
	out["input"] = convertMessagesToInputSlice(req.Messages)
	normalizeResponsesContentPartTypes(out)
	normalizeResponsesInputMessageContent(out)
	normalizeResponsesInputItemIDs(out)
	stripResponsesInputInternalMetadata(out)

	// 2. reasoning effort + summary
	// Explicitly request summary from Codex, otherwise the upstream will not send response.reasoning_summary_text.delta,
	// and chat/completions clients will not get the thinking content (issue #156).
	if effort := normalizeReasoningEffortForModel(req.ReasoningEffort, req.Model); effort != "" {
		out["reasoning"] = map[string]any{
			"effort":  effort,
			"summary": "auto",
		}
	} else {
		out["reasoning"] = map[string]any{"summary": "auto"}
	}

	// 3. service tier (compatibility client field; only fast/priority are explicitly passed to the Codex upstream)
	tier := req.ServiceTier
	if tier == "" {
		tier = req.ServiceTierAlt
	}
	tier = strings.TrimSpace(tier)
	if isAllowedServiceTier(tier) {
		if upstreamTier, ok := upstreamServiceTier(tier); ok {
			out["service_tier"] = upstreamTier
		}
	}

	// 4. tools format conversion + schema cleanup
	if len(req.Tools) > 0 {
		if tools := convertToolsToCodexFormat(req.Tools); len(tools) > 0 {
			out["tools"] = tools
		}
	}

	if choice := gjson.ParseBytes(req.ToolChoice); choice.Get("type").String() == "custom" {
		name := choice.Get("custom.name").String()
		if name == "" {
			name = choice.Get("name").String()
		}
		out["tool_choice"] = map[string]any{"type": "custom", "name": name}
	}

	// 5. response_format → Responses text.format, and clean up the structured output schema
	if len(req.ResponseFormat) > 0 && string(req.ResponseFormat) != "null" {
		var responseFormat map[string]any
		if json.Unmarshal(req.ResponseFormat, &responseFormat) == nil && responseFormat != nil {
			out["response_format"] = responseFormat
			normalizeResponsesStructuredOutputFormat(out)
			delete(out, "response_format")
		}
	}

	return out
}

func copyRawJSONField(out map[string]any, name string, raw json.RawMessage) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return
	}
	var value any
	if json.Unmarshal(raw, &value) == nil {
		out[name] = value
	}
}

func copyChatResponsesControls(out map[string]any, req openAIRequest) error {
	copyRawJSONField(out, "temperature", req.Temperature)
	copyRawJSONField(out, "top_p", req.TopP)
	copyRawJSONField(out, "stop", req.Stop)
	copyRawJSONField(out, "seed", req.Seed)
	copyRawJSONField(out, "presence_penalty", req.PresencePenalty)
	copyRawJSONField(out, "frequency_penalty", req.FrequencyPenalty)
	copyRawJSONField(out, "parallel_tool_calls", req.ParallelToolCalls)

	// max_completion_tokens is the newer Chat spelling and therefore wins when
	// both aliases are supplied.
	maxOutputTokens := req.MaxTokens
	if len(req.MaxCompletionTokens) > 0 && strings.TrimSpace(string(req.MaxCompletionTokens)) != "null" {
		maxOutputTokens = req.MaxCompletionTokens
	}
	copyRawJSONField(out, "max_output_tokens", maxOutputTokens)

	if len(req.ToolChoice) == 0 || strings.TrimSpace(string(req.ToolChoice)) == "null" {
		return nil
	}
	var choice any
	if json.Unmarshal(req.ToolChoice, &choice) != nil {
		return fmt.Errorf("tool_choice must be a valid Chat Completions tool selection")
	}
	// Chat names a selected function under tool_choice.function.name whereas
	// Responses expects {type:"function",name:"..."}.
	switch typed := choice.(type) {
	case string:
		switch strings.TrimSpace(typed) {
		case "auto", "none", "required":
			out["tool_choice"] = typed
			return nil
		default:
			return fmt.Errorf("Chat Completions tool_choice %q cannot be represented by Responses", typed)
		}
	case map[string]any:
		toolType := strings.TrimSpace(firstNonEmptyAnyString(typed["type"]))
		if toolType != "function" && toolType != "custom" {
			return fmt.Errorf("Chat Completions tool_choice type %q cannot be represented by Responses", firstNonEmptyAnyString(typed["type"]))
		}
		name := strings.TrimSpace(firstNonEmptyAnyString(typed["name"]))
		if function, ok := typed[toolType].(map[string]any); ok && name == "" {
			name = strings.TrimSpace(firstNonEmptyAnyString(function["name"]))
		}
		if name == "" {
			return fmt.Errorf("Chat Completions function tool_choice requires function.name")
		}
		out["tool_choice"] = map[string]any{"type": toolType, "name": name}
		return nil
	default:
		return fmt.Errorf("Chat Completions tool_choice cannot be represented by Responses")
	}
}

func chatResponseFormatToResponses(raw json.RawMessage) map[string]any {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil
	}
	var responseFormat map[string]any
	if json.Unmarshal(raw, &responseFormat) != nil || responseFormat == nil {
		return nil
	}
	return responsesTextFormatFromResponseFormat(responseFormat)
}

func invalidFunctionNameError(path string) error {
	return fmt.Errorf("Invalid '%s': empty string. Expected a string with minimum length 1, but got an empty string instead.", path)
}

func validateChatCompletionFunctionNames(req openAIRequest) error {
	knownCalls := make(map[string]struct{})
	for msgIdx, msg := range req.Messages {
		if msg.Role == "tool" {
			callID := strings.TrimSpace(msg.ToolCallID)
			if callID == "" {
				return fmt.Errorf("messages[%d] orphan tool message without tool_call_id", msgIdx)
			}
			if _, ok := knownCalls[callID]; !ok {
				return fmt.Errorf("messages[%d] orphan tool message for unknown tool_call_id %q", msgIdx, callID)
			}
		}
		for callIdx, toolCall := range msg.ToolCalls {
			name, field := toolCall.Function.Name, "function.name"
			if toolCall.Type == "custom" {
				name, field = "", "custom.name"
				if toolCall.Custom != nil {
					name = toolCall.Custom.Name
				}
			}
			if strings.TrimSpace(name) == "" {
				return invalidFunctionNameError(fmt.Sprintf("messages[%d].tool_calls[%d].%s", msgIdx, callIdx, field))
			}
			if callID := strings.TrimSpace(toolCall.ID); msg.Role == "assistant" && callID != "" {
				knownCalls[callID] = struct{}{}
			}
		}
	}
	for toolIdx, rawTool := range req.Tools {
		if tool := gjson.ParseBytes(rawTool); tool.Get("type").String() == "custom" {
			name, field := tool.Get("name").String(), "name"
			if tool.Get("custom").IsObject() {
				name, field = tool.Get("custom.name").String(), "custom.name"
			}
			if strings.TrimSpace(name) == "" {
				return invalidFunctionNameError(fmt.Sprintf("tools[%d].%s", toolIdx, field))
			}
			continue
		}
		var parsed openAIToolParsed
		if err := json.Unmarshal(rawTool, &parsed); err != nil || parsed.Type != "function" || parsed.Function == nil {
			continue
		}
		if strings.TrimSpace(parsed.Function.Name) == "" {
			return invalidFunctionNameError(fmt.Sprintf("tools[%d].function.name", toolIdx))
		}
	}
	return nil
}

// ValidateResponsesFunctionNames rejects malformed tool-call names before they
// reach the upstream Responses API. The upstream reports these as HTTP 400
// empty_string errors; local validation makes the bad client field obvious.
// ValidateResponsesFunctionNames checks that the name of each function_call in input[] and each
// function tool in tools[] is non-empty. It uses lazy gjson traversal and walks only the input and tools arrays,
// rather than deserializing the whole request body into map[string]any: a full Unmarshal on a large request body (once 16MB)
// costs seconds, and the prepare stage then parses it once more (issue #417).
func ValidateResponsesFunctionNames(rawBody []byte) error {
	// For invalid JSON, the gjson query returns an empty result and validation passes directly (consistent with the old behavior where an Unmarshal failure
	// lets it through), so no full ValidBytes scan is needed first.
	var funcErr error
	jsonbody.GetView(rawBody, "input").ForEach(func(idx, item gjson.Result) bool {
		if strings.TrimSpace(item.Get("type").String()) != "function_call" {
			return true
		}
		if strings.TrimSpace(item.Get("name").String()) == "" {
			funcErr = invalidFunctionNameError(fmt.Sprintf("input[%d].name", idx.Int()))
			return false
		}
		return true
	})
	if funcErr != nil {
		return funcErr
	}

	gjson.GetBytes(rawBody, "tools").ForEach(func(idx, tool gjson.Result) bool {
		if strings.TrimSpace(tool.Get("type").String()) != "function" {
			return true
		}
		name := strings.TrimSpace(tool.Get("name").String())
		if name == "" {
			name = strings.TrimSpace(tool.Get("function.name").String())
		}
		if name == "" {
			path := fmt.Sprintf("tools[%d].name", idx.Int())
			if tool.Get("function").IsObject() {
				path = fmt.Sprintf("tools[%d].function.name", idx.Int())
			}
			funcErr = invalidFunctionNameError(path)
			return false
		}
		return true
	})
	return funcErr
}

func normalizeResponsesFunctionTools(body map[string]any) bool {
	tools, ok := body["tools"].([]any)
	if !ok {
		return false
	}
	kept, modified := normalizeFunctionToolsInArray(tools)
	if modified {
		body["tools"] = kept
	}
	return modified
}

// normalizeFunctionToolsInArray handles the "shape" problems of a group of tool declarations: filling in a missing type,
// flattening the Chat-shaped function sub-object to the top level, and dropping unrecognized items. It must run
// before normalizeResponsesToolSchema, because the latter relies on the top-level type to decide whether a tool is a function tool,
// and without a type it cannot get the object fallback for the parameters root node.
func normalizeFunctionToolsInArray(tools []any) ([]any, bool) {
	modified := false
	kept := make([]any, 0, len(tools))
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			kept = append(kept, rawTool)
			continue
		}
		// Pass the tool through as-is, without flattening the function sub-object or rewriting fields (issue #342).
		if isReservedCodexTool(tool) {
			kept = append(kept, tool)
			continue
		}
		toolType := strings.TrimSpace(firstNonEmptyAnyString(tool["type"]))
		if toolType == "" {
			// The upstream returns 400 "Unsupported tool
			// type: None" for a missing tool type or a null one (issue #219). Tools with a function shape (a function sub-object
			// or a top-level name) are treated as function per the OpenAI SDK convention; tools whose shape is not recognized
			// are dropped outright, so the whole request is not rejected by the upstream.
			function, _ := tool["function"].(map[string]any)
			if function == nil &&
				strings.TrimSpace(firstNonEmptyAnyString(tool["name"])) == "" {
				modified = true
				continue
			}
			tool["type"] = "function"
			modified = true
		} else if toolType != "function" {
			kept = append(kept, tool)
			continue
		}
		kept = append(kept, tool)
		function, _ := tool["function"].(map[string]any)
		if function == nil {
			continue
		}
		if strings.TrimSpace(firstNonEmptyAnyString(tool["name"])) == "" {
			if name := strings.TrimSpace(firstNonEmptyAnyString(function["name"])); name != "" {
				tool["name"] = name
				modified = true
			}
		}
		if _, ok := tool["description"]; !ok {
			if desc := strings.TrimSpace(firstNonEmptyAnyString(function["description"])); desc != "" {
				tool["description"] = desc
				modified = true
			}
		}
		if _, ok := tool["parameters"]; !ok {
			if params, ok := function["parameters"]; ok {
				tool["parameters"] = params
				modified = true
			}
		}
		if _, ok := tool["strict"]; !ok {
			if strict, ok := function["strict"]; ok {
				tool["strict"] = strict
			} else {
				// Chat-shaped tools (with a function sub-object) follow the Chat default: strict=false.
				tool["strict"] = false
			}
			modified = true
		}
		delete(tool, "function")
		modified = true
	}
	return kept, modified
}

func normalizeResponsesToolChoice(body map[string]any) bool {
	rawChoice, ok := body["tool_choice"]
	if !ok {
		return false
	}
	choice, ok := rawChoice.(map[string]any)
	if !ok {
		return false
	}

	modified := false
	toolType := strings.TrimSpace(firstNonEmptyAnyString(choice["type"]))
	if toolType == "custom" {
		if nested, ok := choice["custom"].(map[string]any); ok {
			if _, exists := choice["name"]; !exists {
				choice["name"] = nested["name"]
			}
			delete(choice, "custom")
			return true
		}
		return false
	}
	function, _ := choice["function"].(map[string]any)
	name := strings.TrimSpace(firstNonEmptyAnyString(choice["name"]))
	if toolType == "" && (function != nil || name != "") {
		choice["type"] = "function"
		toolType = "function"
		modified = true
	}
	if toolType != "function" {
		return modified
	}
	if name == "" && function != nil {
		if nestedName := strings.TrimSpace(firstNonEmptyAnyString(function["name"])); nestedName != "" {
			choice["name"] = nestedName
			modified = true
		}
	}
	if function != nil {
		delete(choice, "function")
		modified = true
	}
	return modified
}

type responsesBodyPrepareOptions struct {
	forceStoreFalse              bool
	expandPreviousResponse       bool
	preservePreviousResponseID   bool
	deferStructuredStringLengths bool
	skipExpandedInput            bool
	naturalImageIntent           *bool
	cachedResponseItems          []json.RawMessage
	// cacheOwner is the cache ownership namespace used when expanding previous_response_id
	// (see responseCacheOwner). A cache whose owner does not match is treated as a miss, to prevent cross-user injection.
	cacheOwner string
}

// PrepareResponsesBody converts a raw Responses API request into a format the upstream accepts
// uses the Unmarshal → map operations → Marshal pattern, replacing field-by-field sjson operations
// Returns: (processed body, raw text of the expanded input JSON)
func PrepareResponsesBody(rawBody []byte) ([]byte, string) {
	return PrepareResponsesBodyForOwner(rawBody, "")
}

// PrepareResponsesBodyForOwner is the same as PrepareResponsesBody, but the expansion of previous_response_id
// is limited to the owner's cache namespace (see responseCacheOwner).
func PrepareResponsesBodyForOwner(rawBody []byte, owner string) ([]byte, string) {
	prepared := prepareResponsesBodyForOwnerDetailed(rawBody, owner)
	return prepared.Body, prepared.ExpandedInputRaw
}

type responsesBodyPreparation struct {
	Body                 []byte
	ExpandedInputRaw     string
	PreviousResponseID   string
	CacheLookup          responseCacheLookupResult
	RequiresLocalContext bool
	Bypassed             bool
	InputReplayConflict  bool
}

func prepareResponsesBodyForOwnerDetailed(rawBody []byte, owner string) responsesBodyPreparation {
	prevID := gjson.GetBytes(rawBody, "previous_response_id").String()
	preparation := responsesBodyPreparation{PreviousResponseID: prevID}
	if prevID != "" {
		// Codex HTTP removes previous_response_id before sending upstream. Every
		// reference therefore needs an owner-scoped snapshot, even when the new
		// input is plain text or also contains complete tool call/result pairs.
		// Otherwise a cache miss silently turns a continuation into a new turn.
		preparation.RequiresLocalContext = true
		preparation.CacheLookup = getResponseCacheForReplay(owner, prevID)
		if preparation.CacheLookup.Kind == responseCacheLookupHit {
			// A new tool pair is not a replay of the parent. Skip expansion only
			// when overlapping call IDs prove an exact, complete parent prefix.
			preparation.Bypassed, preparation.InputReplayConflict = responsesHTTPClientReplaysParent(
				preparation.CacheLookup.Items, gjson.GetBytes(rawBody, "input"))
		}
	}
	cachedItems := preparation.CacheLookup.Items
	if preparation.Bypassed {
		cachedItems = nil
	}
	preparedBody, expandedInputRaw := prepareResponsesBodyWithOptions(rawBody, responsesBodyPrepareOptions{
		forceStoreFalse:        true,
		expandPreviousResponse: false,
		cacheOwner:             owner,
		cachedResponseItems:    cachedItems,
	})
	preparation.Body = preparedBody
	preparation.ExpandedInputRaw = expandedInputRaw
	return preparation
}

// PrepareResponsesWebSocketBody keeps upstream response storage linkage for
// native Responses WebSocket sessions. ExecuteRequest applies the final schema
// policy once payload rules, account capabilities, and transport are known.
func PrepareResponsesWebSocketBody(rawBody []byte) ([]byte, string) {
	return prepareResponsesBodyWithOptions(rawBody, responsesBodyPrepareOptions{
		preservePreviousResponseID:   true,
		deferStructuredStringLengths: true,
	})
}

// The native turn only needs the outbound body. Preserve the public preparation
// function's replay-input result for callers that actually consume it.
func prepareResponsesWebSocketTurnBody(rawBody []byte) ([]byte, bool) {
	var naturalImageIntent bool
	body, _ := prepareResponsesBodyWithOptions(rawBody, responsesBodyPrepareOptions{
		preservePreviousResponseID:   true,
		deferStructuredStringLengths: true,
		skipExpandedInput:            true,
		naturalImageIntent:           &naturalImageIntent,
	})
	return body, naturalImageIntent
}

const codexReasoningEncryptedContentInclude = "reasoning.encrypted_content"

func ensureDefaultCodexInclude(body map[string]any) {
	if body == nil {
		return
	}
	if _, ok := body["include"]; !ok {
		body["include"] = []string{codexReasoningEncryptedContentInclude}
	}
}

func ensureCodexReasoningInclude(body map[string]any) {
	if body == nil {
		return
	}
	if _, ok := body["reasoning"]; !ok {
		return
	}
	if _, ok := body["include"]; !ok {
		body["include"] = []string{codexReasoningEncryptedContentInclude}
		return
	}

	switch includes := body["include"].(type) {
	case []any:
		for _, item := range includes {
			if s, ok := item.(string); ok && s == codexReasoningEncryptedContentInclude {
				return
			}
		}
		body["include"] = append(includes, codexReasoningEncryptedContentInclude)
	case []string:
		for _, item := range includes {
			if item == codexReasoningEncryptedContentInclude {
				return
			}
		}
		body["include"] = append(includes, codexReasoningEncryptedContentInclude)
	}
}

func prepareResponsesBodyWithOptions(rawBody []byte, opts responsesBodyPrepareOptions) ([]byte, string) {
	var body map[string]any
	if err := json.Unmarshal(rawBody, &body); err != nil {
		return rawBody, ""
	}
	if background, present := body["background"]; present && (background == nil || background == false) {
		// Codex executes synchronously; these nullable/default values request
		// the same behavior and need no unsupported background parameter.
		delete(body, "background")
	}
	if opts.naturalImageIntent != nil {
		// Inspect the original prompt before compatibility rewrites or automatic
		// image-tool injection. Reuse this parse when selecting the transport.
		*opts.naturalImageIntent = promptTextRequestsImageGeneration(extractResponsesPromptText(body))
	}

	// 1. Force-set the fields Codex requires
	body["stream"] = true
	if opts.forceStoreFalse {
		body["store"] = false
	}
	ensureDefaultCodexInclude(body)

	normalizeResponsesImageOnlyModel(body)
	normalizeResponsesPromptCompat(body)

	// 2. A string input is wrapped in an array (Codex requires input to be a list)
	if inputStr, ok := body["input"].(string); ok {
		body["input"] = []any{
			map[string]any{"role": "user", "content": inputStr},
		}
	}
	promptText := extractResponsesPromptText(body)

	// 3. Automatic conversion of reasoning_effort → reasoning.effort, with clamping (max is allowed per model)
	effortModel := firstNonEmptyAnyString(body["model"])
	if re, ok := body["reasoning_effort"].(string); ok {
		if normalized := normalizeReasoningEffortForModel(re, effortModel); normalized != "" {
			reasoning, _ := body["reasoning"].(map[string]any)
			if reasoning == nil {
				reasoning = map[string]any{}
			}
			if _, hasEffort := reasoning["effort"]; !hasEffort {
				reasoning["effort"] = normalized
				body["reasoning"] = reasoning
			}
		}
	}
	if reasoning, ok := body["reasoning"].(map[string]any); ok {
		if effort, ok := reasoning["effort"].(string); ok {
			if normalized := normalizeReasoningEffortForModel(effort, effortModel); normalized != "" {
				reasoning["effort"] = normalized
			} else {
				delete(reasoning, "effort")
			}
		}
	}
	ensureCodexReasoningInclude(body)

	// 4. service tier cleanup (compatibility client field; only fast/priority are explicitly passed to the Codex upstream)
	delete(body, "serviceTier")
	if tier, ok := body["service_tier"].(string); ok {
		tier = strings.TrimSpace(tier)
		if !isAllowedServiceTier(tier) {
			delete(body, "service_tier")
		} else if upstreamTier, ok := upstreamServiceTier(tier); ok {
			body["service_tier"] = upstreamTier
		} else {
			delete(body, "service_tier")
		}
	}
	// Native WS payload rules may enable Lite after this preparation step.
	// Keep length constraints until ExecuteRequest can decide whether they apply.
	normalizeResponsesStructuredOutputFormatWithPolicy(body, schemaNormalizationPolicy{
		preserveStringLengths: opts.deferStructuredStringLengths,
	})
	normalizeResponsesFunctionTools(body)
	normalizeResponsesToolChoice(body)
	normalizeResponsesWebSearchTools(body)

	// 5. Tool description supplement + schema cleanup + upstream count limit
	if tools, ok := body["tools"].([]any); ok {
		if len(tools) > maxTools {
			tools = truncateToolsPreservingImageGeneration(tools)
			body["tools"] = tools
		}
		toolDescDefaults := map[string]string{
			"tool_search": "Search through available tools to find the most relevant one for the task.",
		}
		for _, t := range tools {
			normalizeResponsesToolSchema(t, toolDescDefaults)
		}
	}
	// Responses Lite moves tool declarations into the additional_tools carrier items in input[]; the top-level
	// tools[] cleanup must cover those too, otherwise bad schemas bypass all fixes and go straight upstream.
	normalizeResponsesAdditionalToolSchemas(body)
	if shouldAutoInjectResponsesImageGenerationTool(body) {
		ensureResponsesImageGenerationTool(body)
	}
	moveTopLevelResponsesImageOptions(body)
	normalizeResponsesImageGenerationTools(body, promptText)
	applyResponsesImageGenerationBridgeInstructions(body)

	// 6. Expand previous_response_id (limited to the cache namespace owned by the request, to prevent cross-user injection)
	prevID, _ := body["previous_response_id"].(string)
	if prevID != "" {
		cached := opts.cachedResponseItems
		if opts.expandPreviousResponse && cached == nil {
			cached = getResponseCache(opts.cacheOwner, prevID)
		}
		if cached != nil {
			var cachedItems []any
			for _, item := range cached {
				var v any
				if json.Unmarshal(item, &v) == nil {
					cachedItems = append(cachedItems, v)
				}
			}
			currentInput, _ := body["input"].([]any)
			body["input"] = append(cachedItems, currentInput...)
		}
	}
	// 6b. Translate compaction items in input[] into developer messages (the upstream does not recognize compaction)
	normalizeResponsesCompactionItems(body)
	// system role message → developer (the upstream does not accept the system role, issue #409)
	normalizeResponsesSystemRoleMessages(body)
	normalizeResponsesContentPartTypes(body)
	normalizeResponsesInputMessageContent(body)
	normalizeResponsesToolCallArgumentTypes(body)
	sanitizeMalformedResponsesFunctionCalls(body)
	normalizeResponsesInputItemIDs(body)
	stripResponsesInputInternalMetadata(body)
	dropBareReasoningInputItems(body)
	// 6c. Repair the call_id pairing of tool calls and outputs (issue #414).
	// Skipped for the native continuation scenario where previous_response_id is passed to the upstream: history lives on the upstream server,
	// so output items that look orphaned locally are legitimate continuations and must not be rewritten.
	if !(opts.preservePreviousResponseID && prevID != "") {
		repairResponsesToolCallPairing(body)
	}

	// 7. Delete fields Codex does not support
	// Note: prompt_cache_retention is rejected by the upstream (on both the HTTP and WS paths) with
	// 400 Unsupported parameter, so it is stripped here, and the executor / wsrelay layers each also delete it as a fallback.
	// The top-level type is a field of the Responses WS event envelope (response.create); the native WS ingress
	// injects and keeps it, but the HTTP /responses upstream does not accept it (400 Unsupported parameter: type);
	// WS outbound is reset by wsrelay uniformly, so deleting it here has no effect on the WS path (issue #548).
	// Deleting from the map only affects the top level; legitimate type fields inside nested objects such as input[] and tools are unaffected.
	for _, field := range []string{
		"max_output_tokens", "max_tokens", "max_completion_tokens",
		"temperature", "top_p", "frequency_penalty", "presence_penalty",
		"logprobs", "top_logprobs", "n", "seed", "stop", "user",
		"logit_bias", "response_format", "serviceTier", "metadata",
		"stream_options", "reasoning_effort", "truncation", "context_management",
		"disable_response_storage", "verbosity",
		"prompt_cache_retention", "safety_identifier", "type",
	} {
		delete(body, field)
	}
	if !opts.preservePreviousResponseID {
		delete(body, "previous_response_id")
	}

	result, err := json.Marshal(body)
	if err != nil {
		if opts.skipExpandedInput {
			return rawBody, ""
		}
		var expandedInputRaw string
		if input, ok := body["input"]; ok {
			if encoded, inputErr := json.Marshal(input); inputErr == nil {
				expandedInputRaw = string(encoded)
			}
		}
		return rawBody, expandedInputRaw
	}
	result = normalizeCompactionTriggerFinal(result, false)
	if opts.skipExpandedInput {
		return result, ""
	}
	// Reuse the serialized input, including any final compaction adjustment.
	// Serializing the same input tree separately doubles work on long histories.
	return result, gjson.GetBytes(result, "input").Raw
}

// PrepareCompactResponsesBody converts a /responses/compact request into a format the upstream accepts.
// It reuses the general Responses preprocessing, but removes the automatically injected fields that the compact endpoint does not accept.
func PrepareCompactResponsesBody(rawBody []byte) ([]byte, string) {
	return PrepareCompactResponsesBodyForOwner(rawBody, "")
}

// PrepareCompactResponsesBodyForOwner is the same as PrepareCompactResponsesBody, but
// the expansion of previous_response_id is limited to the owner's cache namespace.
func PrepareCompactResponsesBodyForOwner(rawBody []byte, owner string) ([]byte, string) {
	prepared := prepareCompactResponsesBodyForOwnerDetailed(rawBody, owner)
	return prepared.Body, prepared.ExpandedInputRaw
}

func prepareCompactResponsesBodyForOwnerDetailed(rawBody []byte, owner string) responsesBodyPreparation {
	prepared := prepareResponsesBodyForOwnerDetailed(rawBody, owner)
	prepared.Body, _ = sjson.DeleteBytes(prepared.Body, "include")
	prepared.Body, _ = sjson.DeleteBytes(prepared.Body, "store")
	prepared.Body, _ = sjson.DeleteBytes(prepared.Body, "stream")
	// The client fingerprint metadata carried by ordinary /responses requests is not recognized by the compact endpoint
	// (Unknown parameter: 'client_metadata'), and the body-signal compression promotion sends a body with an ordinary request shape into
	// this function, so it must be stripped here.
	prepared.Body, _ = sjson.DeleteBytes(prepared.Body, "client_metadata")
	return prepared
}

// normalizeReasoningEffort clamps reasoning_effort to the values the upstream supports.
// max is supported only by models from gpt-5.6 onward (older models return 400 upstream); with no model context it is safely clamped to xhigh;
// callers with model context use normalizeReasoningEffortForModel.
func normalizeReasoningEffort(effort string) string {
	effort = strings.ToLower(strings.TrimSpace(effort))
	if effort == "" {
		return ""
	}
	switch effort {
	case "none", "minimal", "low", "medium", "high", "xhigh", "ultra":
		return effort
	case "max":
		return "xhigh"
	default:
		return "high"
	}
}

// normalizeReasoningEffortForModel allows max per model on top of the general clamp:
// from gpt-5.6 onward the upstream accepts effort=max and echoes it as-is; older models return
// "Invalid value: 'max'", and those are always clamped to xhigh.
func normalizeReasoningEffortForModel(effort, model string) string {
	if strings.ToLower(strings.TrimSpace(effort)) == "max" && modelSupportsMaxReasoningEffort(model) {
		return "max"
	}
	return normalizeReasoningEffort(effort)
}

// modelSupportsMaxReasoningEffort reports whether the model supports reasoning.effort=max
// (gpt-5.6 and later; variant suffixes such as gpt-5.6-sol are recognized too).
// The stable gpt-daybreak-*-latest aliases of Trusted Access for Cyber point to the 5.6 family
// (blue=gpt-5.6-sol, red=gpt-5.6-cyber, issue #624), and are allowed too.
func modelSupportsMaxReasoningEffort(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	if !strings.HasPrefix(model, "gpt-") {
		return false
	}
	if strings.HasPrefix(model, "gpt-daybreak-") {
		return true
	}
	version := strings.TrimPrefix(model, "gpt-")
	if dash := strings.IndexByte(version, '-'); dash >= 0 {
		version = version[:dash]
	}
	parts := strings.Split(version, ".")
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return false
	}
	// Only new-generation models with a bare major version (gpt-6-astra, gpt-6) are listed on the official model page with
	// the Max tier, so they are allowed when major > 5; a missing ".x" must not be treated as an old model and clamped.
	if major > 5 {
		return true
	}
	if len(parts) < 2 {
		return false
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return false
	}
	return major == 5 && minor >= 6
}

// isAllowedServiceTier reports whether service_tier is within the range the upstream allows
func isAllowedServiceTier(tier string) bool {
	switch tier {
	case "auto", "default", "flex", "priority", "scale", "fast", "ultrafast":
		return true
	default:
		return false
	}
}

// upstreamServiceTier maps the client service_tier to the value the upstream accepts.
// priority/ultrafast keep their tiers; auto/default/flex/scale follow the existing omission policy.
func upstreamServiceTier(tier string) (string, bool) {
	switch tier {
	case "fast", "priority":
		return "priority", true
	case "ultrafast":
		return "ultrafast", true
	case "auto", "default", "flex", "scale":
		return "", false
	default:
		return "", false
	}
}

// convertMessagesToInputSlice converts OpenAI messages into the Codex input array (pure in-memory operation, zero intermediate serialization)
func convertMessagesToInputSlice(messages []openAIMessage) []any {
	input := make([]any, 0, len(messages))
	customCalls := make(map[string]bool)
	for _, message := range messages {
		for _, call := range message.ToolCalls {
			if call.Type == "custom" {
				customCalls[call.ID] = true
			}
		}
	}
	for _, m := range messages {
		switch m.Role {
		case "tool":
			output, images := toolMessageOutputAndImages(m.Content)
			outputType := "function_call_output"
			if customCalls[m.ToolCallID] {
				outputType = "custom_tool_call_output"
			}
			input = append(input, map[string]any{
				"type":    outputType,
				"call_id": m.ToolCallID,
				"output":  output,
			})
			// function_call_output can only be text; images in an array content are extracted and put into
			// the synthetic user message that immediately follows, with a note about call ownership.
			if len(images) > 0 {
				mediaParts := []any{map[string]any{
					"type": "input_text",
					"text": fmt.Sprintf(toolResultImageAttribution, m.ToolCallID),
				}}
				for _, url := range images {
					mediaParts = append(mediaParts, map[string]any{"type": "input_image", "image_url": url})
				}
				input = append(input, map[string]any{
					"type":    "message",
					"role":    "user",
					"content": mediaParts,
				})
			}

		case "assistant":
			if len(m.ToolCalls) > 0 {
				// assistant message with tool_calls
				if text := rawMessageToString(m.Content); text != "" {
					input = append(input, map[string]any{
						"type": "message",
						"role": "assistant",
						"content": []any{
							map[string]any{"type": "output_text", "text": text},
						},
					})
				}
				for _, tc := range m.ToolCalls {
					if tc.Type == "custom" && tc.Custom != nil {
						item := map[string]any{"type": "custom_tool_call", "call_id": tc.ID, "name": tc.Custom.Name, "input": tc.Custom.Input}
						if tc.Namespace != "" {
							item["namespace"] = tc.Namespace
						}
						input = append(input, item)
						continue
					}
					input = append(input, map[string]any{
						"type":      "function_call",
						"call_id":   tc.ID,
						"name":      tc.Function.Name,
						"arguments": tc.Function.Arguments,
					})
				}
			} else {
				input = append(input, map[string]any{
					"type":    "message",
					"role":    "assistant",
					"content": buildContentPartsSlice("assistant", m.Content),
				})
			}

		case "system":
			input = append(input, map[string]any{
				"type":    "message",
				"role":    "developer",
				"content": buildContentPartsSlice("system", m.Content),
			})

		default:
			input = append(input, map[string]any{
				"type":    "message",
				"role":    "user",
				"content": buildContentPartsSlice("user", m.Content),
			})
		}
	}
	return input
}

// buildContentPartsSlice converts content (string or []contentPart) into []any
func buildContentPartsSlice(role string, raw json.RawMessage) []any {
	parts := make([]any, 0)
	if len(raw) == 0 {
		return parts
	}

	contentType := "input_text"
	if role == "assistant" {
		contentType = "output_text"
	}

	first := firstNonSpace(raw)
	switch first {
	case '"':
		var s string
		if json.Unmarshal(raw, &s) != nil || s == "" {
			return parts
		}
		return append(parts, map[string]any{"type": contentType, "text": s})
	case '[':
		var arr []openAIContentPart
		if json.Unmarshal(raw, &arr) != nil {
			return parts
		}
		for _, item := range arr {
			switch item.Type {
			case "text":
				parts = append(parts, map[string]any{"type": contentType, "text": item.Text})
			case "image_url":
				if item.ImageURL != nil && item.ImageURL.URL != "" {
					parts = append(parts, map[string]any{"type": "input_image", "image_url": item.ImageURL.URL})
				}
			case "file":
				if item.File != nil && (strings.TrimSpace(item.File.FileID) != "" || strings.TrimSpace(item.File.FileData) != "") {
					part := map[string]any{"type": "input_file"}
					if filename := strings.TrimSpace(item.File.Filename); filename != "" {
						part["filename"] = filename
					}
					if fileData := strings.TrimSpace(item.File.FileData); fileData != "" {
						part["file_data"] = fileData
					}
					if fileID := strings.TrimSpace(item.File.FileID); fileID != "" {
						part["file_id"] = fileID
					}
					parts = append(parts, part)
				}
			}
		}
		return parts
	default:
		return parts
	}
}

// toolMessageOutputAndImages computes the function_call_output text of a tool message, and extracts
// the image URLs in array content. When there are no images, output is exactly the same as rawMessageToString (including the array's
// raw bytes), so the prompt-cache prefix of image-free requests is unchanged.
func toolMessageOutputAndImages(raw json.RawMessage) (string, []string) {
	if firstNonSpace(raw) != '[' {
		return rawMessageToString(raw), nil
	}
	var arr []openAIContentPart
	if json.Unmarshal(raw, &arr) != nil {
		return rawMessageToString(raw), nil
	}
	var images []string
	for _, item := range arr {
		if item.Type == "image_url" && item.ImageURL != nil && item.ImageURL.URL != "" {
			images = append(images, item.ImageURL.URL)
		}
	}
	if len(images) == 0 {
		// No images: keep the original byte path, no rewriting.
		return rawMessageToString(raw), nil
	}
	var textParts []string
	for _, item := range arr {
		if item.Type == "text" && item.Text != "" {
			textParts = append(textParts, item.Text)
		}
	}
	output := strings.Join(textParts, "\n")
	if output == "" {
		output = toolResultImageMovedMarker
	}
	return output, images
}

// rawMessageToString safely converts a json.RawMessage to a Go string
func rawMessageToString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

func firstNonSpace(raw json.RawMessage) byte {
	for _, b := range raw {
		if b != ' ' && b != '\n' && b != '\r' && b != '\t' {
			return b
		}
	}
	return 0
}

// convertToolsToCodexFormat converts OpenAI tool format to Codex format (pure in-memory operation)
// OpenAI: {type:"function", function:{name, description, parameters}}
// Codex:  {type:"function", name, description, parameters}
// The upstream limit is 128 tools at most; excess is silently truncated
func convertToolsToCodexFormat(rawTools []json.RawMessage) []any {
	cap := len(rawTools)
	if cap > maxTools {
		cap = maxTools
		rawTools = rawTools[:maxTools]
	}
	tools := make([]any, 0, cap)
	for _, raw := range rawTools {
		var parsed openAIToolParsed
		if json.Unmarshal(raw, &parsed) != nil {
			continue
		}

		// When type is missing or null, treat it as function per the OpenAI SDK convention (provided it has
		// a function object); the upstream returns 400 "Unsupported tool
		// type: None" for an empty type (issue #219), and tools whose shape is not recognized are dropped outright.
		isFunction := parsed.Function != nil &&
			(parsed.Type == "function" || parsed.Type == "")
		if !isFunction {
			if parsed.Type == "" {
				// No function object but a top-level name (Codex format missing type) → fill in
				// the type and keep it; everything else is dropped.
				var toolMap map[string]any
				if json.Unmarshal(raw, &toolMap) == nil &&
					strings.TrimSpace(firstNonEmptyAnyString(toolMap["name"])) != "" {
					toolMap["type"] = "function"
					normalizeFunctionToolParameters(toolMap)
					tools = append(tools, toolMap)
				}
				continue
			}
			// Non-function type → pass the original JSON through
			// Exception: variants such as web_search_preview are normalized to web_search,
			// since the Codex upstream only accepts a bare "web_search". Normalization keeps the allowlisted fields,
			// consistent with the PrepareResponsesBody path.
			if strings.HasPrefix(parsed.Type, "web_search") {
				var toolMap map[string]any
				if json.Unmarshal(raw, &toolMap) == nil && toolMap != nil {
					tools = append(tools, normalizeCodexWebSearchTool(toolMap))
				} else {
					tools = append(tools, map[string]any{"type": "web_search"})
				}
				continue
			}
			var passThrough any
			_ = json.Unmarshal(raw, &passThrough)
			if tool, ok := passThrough.(map[string]any); ok {
				passThrough = lowerChatCustomTool(tool)
			}
			tools = append(tools, passThrough)
			continue
		}

		// function type → promote the fields inside function to the top level
		item := map[string]any{
			"type": "function",
			"name": parsed.Function.Name,
		}
		if parsed.Function.Description != "" {
			item["description"] = parsed.Function.Description
		}
		if len(parsed.Function.Parameters) > 0 {
			var params map[string]any
			if json.Unmarshal(parsed.Function.Parameters, &params) == nil && params != nil {
				item["parameters"] = params
			}
		}
		normalizeFunctionToolParameters(item)
		if parsed.Function.Strict != nil {
			item["strict"] = *parsed.Function.Strict
		} else {
			// Chat Completions strict defaults to false, Responses defaults to true. When omitted,
			// it must be explicitly sent as false, otherwise the upstream validates the schema per strict (optional properties, missing
			// additionalProperties:false, etc.) and returns 400 directly. Codex CLI's own tools also all send false.
			item["strict"] = false
		}
		tools = append(tools, item)
	}
	return tools
}

// ==================== backward compatibility: helper functions ====================

func normalizeServiceTierField(body []byte) []byte {
	tier := strings.TrimSpace(gjson.GetBytes(body, "service_tier").String())
	if tier == "" {
		tier = strings.TrimSpace(gjson.GetBytes(body, "serviceTier").String())
	}
	if tier == "" {
		return body
	}
	body = jsonbody.SetString(body, "service_tier", tier)
	body = jsonbody.DeletePath(body, "serviceTier")
	return body
}

func sanitizeServiceTierForUpstream(body []byte) []byte {
	tier := strings.TrimSpace(gjson.GetBytes(body, "service_tier").String())
	if tier == "" {
		return jsonbody.DeletePath(body, "serviceTier")
	}
	switch tier {
	case "auto", "default", "flex", "priority", "scale", "fast", "ultrafast":
		body = jsonbody.DeletePath(body, "serviceTier")
		if upstreamTier, ok := upstreamServiceTier(tier); ok {
			body = jsonbody.SetString(body, "service_tier", upstreamTier)
		} else {
			body = jsonbody.DeletePath(body, "service_tier")
		}
		return body
	default:
		body = jsonbody.DeletePath(body, "service_tier")
		body = jsonbody.DeletePath(body, "serviceTier")
		return body
	}
}

type usageServiceTiers struct {
	ServiceTier          string
	RequestedServiceTier string
	ActualServiceTier    string
	BillingServiceTier   string
}

func normalizeServiceTierValue(tier string) string {
	return strings.ToLower(strings.TrimSpace(tier))
}

func normalizeDisplayServiceTier(tier string) string {
	tier = normalizeServiceTierValue(tier)
	if tier == "priority" {
		return "fast"
	}
	return tier
}

func normalizeBillingServiceTier(tier string) string {
	tier = normalizeServiceTierValue(tier)
	if tier == "priority" || tier == "fast" {
		return "priority"
	}
	return tier
}

// resolveServiceTier is the legacy service_tier field for usage logs.
// It now prefers the upstream actual tier so response.service_tier="default"
// is not masked by a requested fast/priority intent.
func resolveServiceTier(actualTier, requestedTier string) string {
	if actual := normalizeDisplayServiceTier(actualTier); actual != "" {
		return actual
	}
	return normalizeDisplayServiceTier(requestedTier)
}

func resolveUsageServiceTiers(actualTier, requestedTier string) usageServiceTiers {
	return usageServiceTiers{
		ServiceTier:          resolveServiceTier(actualTier, requestedTier),
		RequestedServiceTier: normalizeServiceTierValue(requestedTier),
		ActualServiceTier:    normalizeServiceTierValue(actualTier),
		BillingServiceTier:   resolveBillingServiceTier(actualTier, requestedTier),
	}
}

// resolveBillingServiceTier selects the tier used for money. The default policy
// treats the requested tier as a ceiling: an upstream observation may lower the
// bill, but it must never raise it. requested policy ignores observations and
// preserves the explicit client intent.
func resolveBillingServiceTier(actualTier, requestedTier string) string {
	return resolveBillingServiceTierForPolicy(actualTier, requestedTier, CurrentRuntimeSettings().BillingTierPolicy)
}

func resolveBillingServiceTierForPolicy(actualTier, requestedTier, policy string) string {
	actualTier = normalizeBillingServiceTier(actualTier)
	requestedTier = normalizeBillingServiceTier(requestedTier)

	if NormalizeBillingTierPolicy(policy) == BillingTierPolicyRequested {
		return requestedTier
	}

	if actualTier == "" || actualTier == requestedTier {
		return requestedTier
	}
	actualRank, actualKnown := billingServiceTierCostRank(actualTier)
	requestedRank, requestedKnown := billingServiceTierCostRank(requestedTier)
	if !actualKnown || !requestedKnown || actualRank >= requestedRank {
		return requestedTier
	}
	return actualTier
}

// billingServiceTierCostRank orders known tiers by relative price. Empty is the
// ordinary base tier: an unsolicited priority observation therefore cannot turn
// an untiered request into a Fast charge. Unknown tiers are never trusted to
// change billing in either direction.
func billingServiceTierCostRank(tier string) (int, bool) {
	switch normalizeBillingServiceTier(tier) {
	case "flex":
		return 0, true
	case "", "default", "standard", "auto", "scale":
		return 1, true
	case "priority":
		return 2, true
	case "ultrafast":
		return 3, true
	default:
		return 1, false
	}
}

// Upstream-unsupported JSON Schema validation constraint keywords
var unsupportedSchemaKeys = map[string]bool{
	"uniqueItems":      true,
	"minItems":         true,
	"maxItems":         true,
	"minimum":          true,
	"maximum":          true,
	"exclusiveMinimum": true,
	"exclusiveMaximum": true,
	"multipleOf":       true,
	"pattern":          true,
	"minLength":        true,
	"maxLength":        true,
	"format":           true,
	"minProperties":    true,
	"maxProperties":    true,
}

// Keywords whose value is a map of name → sub-schema. `definitions` is the draft-07 form,
// which pydantic v1 and older zod-to-json-schema both generate instead of `$defs`.
var schemaSubSchemaMapKeys = []string{
	"properties",
	"patternProperties",
	"dependentSchemas",
	"$defs",
	"definitions",
}

// Keywords whose value is an array of sub-schemas. `prefixItems` is the draft 2020-12 tuple form.
var schemaSubSchemaListKeys = []string{"allOf", "anyOf", "oneOf", "prefixItems"}

// Keywords whose value is a single sub-schema.
var schemaSubSchemaKeys = []string{
	"additionalProperties",
	"not",
	"if",
	"then",
	"else",
	"propertyNames",
	"contains",
}

// forEachSubSchema walks all direct sub-schemas of a schema. All recursive cleanup functions must go through
// here, rather than each copying its own descent logic; otherwise adding a new schema keyword means changing several places, and missing one
// silently lets a bad schema through (this is how `definitions` and tuple-form `items` were missed).
func forEachSubSchema(schema map[string]interface{}, visit func(map[string]interface{})) {
	for _, key := range schemaSubSchemaMapKeys {
		entries, ok := schema[key].(map[string]interface{})
		if !ok {
			continue
		}
		for _, v := range entries {
			if sub, ok := v.(map[string]interface{}); ok {
				visit(sub)
			}
		}
	}
	for _, key := range schemaSubSchemaListKeys {
		arr, ok := schema[key].([]interface{})
		if !ok {
			continue
		}
		for _, item := range arr {
			if sub, ok := item.(map[string]interface{}); ok {
				visit(sub)
			}
		}
	}
	for _, key := range schemaSubSchemaKeys {
		if sub, ok := schema[key].(map[string]interface{}); ok {
			visit(sub)
		}
	}
	// items has two legal forms: a single schema, or an array of schemas for draft-07 tuple validation.
	switch items := schema["items"].(type) {
	case map[string]interface{}:
		visit(items)
	case []interface{}:
		for _, item := range items {
			if sub, ok := item.(map[string]interface{}); ok {
				visit(sub)
			}
		}
	}
}

// stripUnsupportedSchemaKeys recursively deletes keywords from the schema that the upstream does not support
func stripUnsupportedSchemaKeys(schema map[string]interface{}) {
	stripUnsupportedSchemaKeysWithPolicy(schema, schemaNormalizationPolicy{})
}

func stripUnsupportedSchemaKeysWithPolicy(schema map[string]interface{}, policy schemaNormalizationPolicy) {
	for key := range unsupportedSchemaKeys {
		if policy.preserveStringLengths && (key == "minLength" || key == "maxLength") {
			continue
		}
		delete(schema, key)
	}
	// The upstream rejects every explicitly null type (Codex Desktop's automation_update
	// does send it this way). Deleting it falls back to a legal schema with an undeclared type; the root node of a function tool
	// is then also filled in to object by ensureFunctionParametersRootObject. Once a bad schema sinks into
	// multi-turn history, every turn gets a 400 unless it is fixed.
	if rawType, exists := schema["type"]; exists && rawType == nil {
		delete(schema, "type")
	}
	forEachSubSchema(schema, func(child map[string]any) { stripUnsupportedSchemaKeysWithPolicy(child, policy) })
}

func sanitizeSchemaForUpstream(schema map[string]interface{}) {
	simplifyConstUnionSchemas(schema)
	stripUnsupportedSchemaKeys(schema)
	normalizeSchemaRequiredFields(schema)
	ensureArrayItems(schema)
}

func sanitizeStructuredOutputSchemaForUpstream(schema map[string]interface{}) {
	sanitizeStructuredOutputSchemaWithPolicy(schema, schemaNormalizationPolicy{})
}

func sanitizeStructuredOutputSchemaWithPolicy(schema map[string]interface{}, policy schemaNormalizationPolicy) {
	stripUnsupportedSchemaKeysWithPolicy(schema, policy)
	normalizeSchemaRequiredFields(schema)
	ensureArrayItems(schema)
	ensureObjectAdditionalPropertiesFalse(schema)
	alignRequiredWithProperties(schema)
}

func normalizeResponsesStructuredOutputFormat(body map[string]any) bool {
	return normalizeResponsesStructuredOutputFormatWithPolicy(body, schemaNormalizationPolicy{})
}

func normalizeResponsesStructuredOutputFormatWithPolicy(body map[string]any, policy schemaNormalizationPolicy) bool {
	if len(body) == 0 {
		return false
	}

	modified := false
	if responseFormat, ok := body["response_format"].(map[string]any); ok {
		if textFormat := responsesTextFormatFromResponseFormat(responseFormat); textFormat != nil {
			text, _ := body["text"].(map[string]any)
			if text == nil {
				text = map[string]any{}
				body["text"] = text
			}
			if _, hasFormat := text["format"]; !hasFormat {
				text["format"] = textFormat
				modified = true
			}
		}
		if sanitizeStructuredOutputFormatWithPolicy(responseFormat, policy) {
			modified = true
		}
	}

	text, ok := body["text"].(map[string]any)
	if !ok {
		return modified
	}
	format, ok := text["format"].(map[string]any)
	if !ok {
		return modified
	}
	if sanitizeStructuredOutputFormatWithPolicy(format, policy) {
		modified = true
	}
	if ensureJSONModeInputMentionsJSON(body, format) {
		modified = true
	}
	return modified
}

func ensureJSONModeInputMentionsJSON(body map[string]any, format map[string]any) bool {
	if strings.TrimSpace(firstNonEmptyAnyString(format["type"])) != "json_object" {
		return false
	}
	input, ok := body["input"]
	if !ok || responsesInputContainsJSON(input) {
		return false
	}

	switch inputValue := input.(type) {
	case string:
		body["input"] = jsonObjectFormatInputHint + "\n\n" + inputValue
		return true
	case []any:
		body["input"] = append([]any{jsonObjectDeveloperMessage()}, inputValue...)
		return true
	case []map[string]string:
		inputItems := make([]any, 0, len(inputValue)+1)
		inputItems = append(inputItems, jsonObjectDeveloperMessage())
		for _, item := range inputValue {
			inputItems = append(inputItems, item)
		}
		body["input"] = inputItems
		return true
	case []map[string]any:
		inputItems := make([]any, 0, len(inputValue)+1)
		inputItems = append(inputItems, jsonObjectDeveloperMessage())
		for _, item := range inputValue {
			inputItems = append(inputItems, item)
		}
		body["input"] = inputItems
		return true
	default:
		return false
	}
}

func jsonObjectDeveloperMessage() map[string]any {
	return map[string]any{
		"type": "message",
		"role": "developer",
		"content": []any{
			map[string]any{"type": "input_text", "text": jsonObjectFormatInputHint},
		},
	}
}

func responsesInputContainsJSON(value any) bool {
	switch v := value.(type) {
	case string:
		return strings.Contains(strings.ToLower(v), "json")
	case []any:
		for _, item := range v {
			if responsesInputContainsJSON(item) {
				return true
			}
		}
	case []map[string]string:
		for _, item := range v {
			if responsesInputContainsJSON(item) {
				return true
			}
		}
	case []map[string]any:
		for _, item := range v {
			if responsesInputContainsJSON(item) {
				return true
			}
		}
	case map[string]any:
		for _, key := range []string{"content", "text", "output"} {
			if child, ok := v[key]; ok && responsesInputContainsJSON(child) {
				return true
			}
		}
	case map[string]string:
		for _, key := range []string{"content", "text", "output"} {
			if child, ok := v[key]; ok && responsesInputContainsJSON(child) {
				return true
			}
		}
	}
	return false
}

func responsesTextFormatFromResponseFormat(responseFormat map[string]any) map[string]any {
	formatType := strings.TrimSpace(firstNonEmptyAnyString(responseFormat["type"]))
	switch formatType {
	case "json_schema":
		if jsonSchema, ok := responseFormat["json_schema"].(map[string]any); ok && jsonSchema != nil {
			out := make(map[string]any, len(jsonSchema)+1)
			for key, value := range jsonSchema {
				out[key] = value
			}
			out["type"] = "json_schema"
			return out
		}
		out := make(map[string]any, len(responseFormat))
		for key, value := range responseFormat {
			if key == "json_schema" {
				continue
			}
			out[key] = value
		}
		out["type"] = "json_schema"
		return out
	case "json_object", "text":
		return map[string]any{"type": formatType}
	default:
		return nil
	}
}

func sanitizeStructuredOutputSchema(format map[string]any) bool {
	return sanitizeStructuredOutputFormatWithPolicy(format, schemaNormalizationPolicy{})
}

func sanitizeStructuredOutputFormatWithPolicy(format map[string]any, policy schemaNormalizationPolicy) bool {
	modified := false
	if schema, ok := format["schema"].(map[string]any); ok && schema != nil {
		sanitizeStructuredOutputSchemaWithPolicy(schema, policy)
		modified = true
	}
	if jsonSchema, ok := format["json_schema"].(map[string]any); ok && jsonSchema != nil {
		if schema, ok := jsonSchema["schema"].(map[string]any); ok && schema != nil {
			sanitizeStructuredOutputSchemaWithPolicy(schema, policy)
			modified = true
		}
	}
	return modified
}

func isFunctionTool(tool map[string]any) bool {
	toolType, _ := tool["type"].(string)
	return strings.TrimSpace(toolType) == "function"
}

// reservedCodexToolNamePrefixes lists the tool namespaces the upstream reserves for the model. Such tools
// (such as gpt-5.6 multi-agent v2's collaboration.spawn_agent) require the schema to match the upstream's
// official configuration verbatim, and the proxy's general schema cleanup (stripUnsupportedSchemaKeys etc.) breaks the
// match, causing the upstream to return 400 "reserved for use by this model and must match the configured
// schema" (issue #342). Such tools must be passed through unchanged: no descriptions added, no cleanup, no flattening.
var reservedCodexToolNamePrefixes = []string{"collaboration."}

// isReservedCodexTool reports whether a tool name falls in the upstream's reserved namespace.
// Compatible with the flat Responses shape (top-level name) and the nested Chat Completions shape (function.name).
func isReservedCodexTool(tool map[string]any) bool {
	if tool == nil {
		return false
	}
	name := strings.TrimSpace(firstNonEmptyAnyString(tool["name"]))
	if name == "" {
		if fn, ok := tool["function"].(map[string]any); ok {
			name = strings.TrimSpace(firstNonEmptyAnyString(fn["name"]))
		}
	}
	if name == "" {
		return false
	}
	lower := strings.ToLower(name)
	for _, prefix := range reservedCodexToolNamePrefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

// normalizeResponsesToolSchema cleans a single tool declaration: fills in a default description, recursively cleans
// the keywords the upstream does not support in the JSON Schema, and fixes the root structure that function tools require. Reserved tools
// (collaboration.* etc.) are passed through unchanged: the upstream requires their schema to match the official configuration verbatim,
// and any description filling or cleanup breaks the match and gets it rejected (issue #342).
func normalizeResponsesToolSchema(rawTool any, descDefaults map[string]string) {
	toolMap, ok := rawTool.(map[string]any)
	if !ok || isReservedCodexTool(toolMap) {
		return
	}
	// Defaults describe user-defined tools by name. Builtin tool types have
	// their own wire schemas (server-executed tool_search forbids description).
	// Explicit fields, including empty or invalid values, belong to the client.
	toolType, _ := toolMap["type"].(string)
	if toolType == "function" || toolType == "custom" {
		if _, exists := toolMap["description"]; !exists {
			if defaultDesc, ok := descDefaults[strings.TrimSpace(firstNonEmptyAnyString(toolMap["name"]))]; ok {
				toolMap["description"] = defaultDesc
			}
		}
	}
	if isFunctionTool(toolMap) {
		normalizeFunctionToolParameters(toolMap)
	} else if params, ok := toolMap["parameters"].(map[string]any); ok {
		sanitizeSchemaForUpstream(params)
	}
}

// normalizeResponsesAdditionalToolSchemas applies to tool lists embedded in type=additional_tools
// carrier items in input[] (Responses Lite format) the same cleanup as the top-level tools[].
func normalizeResponsesAdditionalToolSchemas(body map[string]any) {
	input, ok := body["input"].([]any)
	if !ok {
		return
	}
	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		if strings.TrimSpace(firstNonEmptyAnyString(item["type"])) != "additional_tools" {
			continue
		}
		tools, ok := item["tools"].([]any)
		if !ok {
			continue
		}
		tools, _ = normalizeFunctionToolsInArray(tools)
		item["tools"] = tools
		for _, rawTool := range tools {
			normalizeResponsesToolSchema(rawTool, nil)
		}
	}
}

func normalizeFunctionToolParameters(tool map[string]any) {
	params, ok := tool["parameters"].(map[string]any)
	if !ok || params == nil {
		tool["parameters"] = defaultFunctionParametersSchema()
		return
	}
	sanitizeSchemaForUpstream(params)
	ensureFunctionParametersRootObject(params)
}

func defaultFunctionParametersSchema() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}

func ensureFunctionParametersRootObject(schema map[string]any) {
	if schemaType, ok := schema["type"].(string); !ok || strings.TrimSpace(schemaType) != "object" {
		schema["type"] = "object"
	}
	if props, ok := schema["properties"].(map[string]any); !ok || props == nil {
		schema["properties"] = map[string]any{}
	}
}

func normalizeSchemaRequiredFields(schema map[string]interface{}) {
	if rawRequired, exists := schema["required"]; exists {
		required, ok := rawRequired.([]interface{})
		if !ok {
			delete(schema, "required")
		} else {
			cleaned := make([]interface{}, 0, len(required))
			for _, item := range required {
				if name, ok := item.(string); ok && strings.TrimSpace(name) != "" {
					cleaned = append(cleaned, name)
				}
			}
			if len(cleaned) == 0 {
				delete(schema, "required")
			} else {
				schema["required"] = cleaned
			}
		}
	}
	forEachSubSchema(schema, normalizeSchemaRequiredFields)
}

// ensureArrayItems recursively fills in an empty schema for array schemas that are missing items,
// to be compatible with the upstream validation that requires array to declare items.
func ensureArrayItems(schema map[string]interface{}) {
	if schemaDeclaresArray(schema) {
		if _, ok := schema["items"]; !ok {
			schema["items"] = map[string]interface{}{}
		}
	}
	forEachSubSchema(schema, ensureArrayItems)
}

func ensureObjectAdditionalPropertiesFalse(schema map[string]interface{}) {
	if schemaDeclaresObject(schema) {
		schema["additionalProperties"] = false
	}
	forEachSubSchema(schema, ensureObjectAdditionalPropertiesFalse)
}

// alignRequiredWithProperties makes every object node with properties satisfy the upstream strict-mode
// validation: required must equal exactly all the keys of properties. Extra required entries are dropped directly
// (in strict mode additionalProperties=false, declaring a required field that does not exist will only be
// rejected with 400 upstream), and missing keys are filled in alphabetical order.
func alignRequiredWithProperties(schema map[string]interface{}) {
	if props, ok := schema["properties"].(map[string]interface{}); ok {
		required := make([]interface{}, 0, len(props))
		seen := make(map[string]bool, len(props))
		if existing, ok := schema["required"].([]interface{}); ok {
			for _, item := range existing {
				name, ok := item.(string)
				if !ok || seen[name] {
					continue
				}
				if _, exists := props[name]; !exists {
					continue
				}
				seen[name] = true
				required = append(required, name)
			}
		}
		missing := make([]string, 0, len(props))
		for name := range props {
			if !seen[name] {
				missing = append(missing, name)
			}
		}
		sort.Strings(missing)
		for _, name := range missing {
			required = append(required, name)
		}
		if len(required) == 0 {
			delete(schema, "required")
		} else {
			schema["required"] = required
		}
	} else {
		pruneRequiredWithoutProperties(schema)
	}
	forEachSubSchema(schema, alignRequiredWithProperties)
}

// schemaCompositionKeys are the composition keywords that may supply fields for a sibling required. allOf branches
// always apply, while anyOf/oneOf/then/else only apply to some instances, but the field names they declare still belong to
// "the fields this node may own", which is enough to judge whether a required entry has a source. not is not among them:
// it describes a forbidden shape and provides no fields.
var schemaCompositionKeys = []string{"allOf", "anyOf", "oneOf", "then", "else"}

// schemaReferencesExternalDefinition reports whether a node puts its field definitions elsewhere through a reference.
// The properties of such a node are in the reference target (the target itself is cleaned separately as a $defs/definitions sub-schema),
// which this function cannot see, so its required must not be pruned based on it.
func schemaReferencesExternalDefinition(schema map[string]interface{}) bool {
	for _, key := range []string{"$ref", "$dynamicRef"} {
		if ref, ok := schema[key].(string); ok && strings.TrimSpace(ref) != "" {
			return true
		}
	}
	return false
}

// collectSchemaPropertyNames collects the field names declared by the node itself and its composition branches.
func collectSchemaPropertyNames(schema map[string]interface{}, names map[string]bool) {
	if props, ok := schema["properties"].(map[string]interface{}); ok {
		for name := range props {
			names[name] = true
		}
	}
	collectCompositionPropertyNames(schema, names)
}

// collectCompositionPropertyNames recursively collects the union of field names declared in composition branches (including nested compositions).
// It is used only to judge whether names in a sibling required have a source; it does not resolve $ref.
func collectCompositionPropertyNames(schema map[string]interface{}, names map[string]bool) {
	for _, key := range schemaCompositionKeys {
		switch branch := schema[key].(type) {
		case map[string]interface{}:
			collectSchemaPropertyNames(branch, names)
		case []interface{}:
			for _, item := range branch {
				if sub, ok := item.(map[string]interface{}); ok {
					collectSchemaPropertyNames(sub, names)
				}
			}
		}
	}
}

// pruneRequiredWithoutProperties handles nodes that declare required but have no properties of their own.
// alignRequiredWithProperties originally aligned required only when the node itself had properties, so the extra entries of such
// nodes were sent upstream unchanged and triggered the strict validation
// `'required' is required to be supplied and to be an array including every key in
// properties. Extra required key 'x' supplied.`
//
// Three dispositions, ordered by the strength of the available evidence:
//   - when a field name is hidden in allOf/anyOf/oneOf/then/else branches, trim the entries with no source by the union of the branches;
//     missing entries are not filled in, since under composition semantics filling them would change the meaning of the schema.
//   - when no source can be found at all and the node claims to be an object, required cannot be satisfied under additionalProperties=false,
//     so the whole required is deleted.
//   - when the node has $ref/$dynamicRef, its field definitions live elsewhere, so it is kept as-is to avoid wrongly deleting entries.
func pruneRequiredWithoutProperties(schema map[string]interface{}) {
	existing, ok := schema["required"].([]interface{})
	if !ok || len(existing) == 0 {
		return
	}
	if schemaReferencesExternalDefinition(schema) {
		return
	}
	names := make(map[string]bool)
	collectCompositionPropertyNames(schema, names)
	if len(names) == 0 {
		if schemaDeclaresObject(schema) {
			delete(schema, "required")
		}
		return
	}
	kept := make([]interface{}, 0, len(existing))
	seen := make(map[string]bool, len(existing))
	for _, item := range existing {
		name, ok := item.(string)
		if !ok || seen[name] || !names[name] {
			continue
		}
		seen[name] = true
		kept = append(kept, name)
	}
	if len(kept) == 0 {
		delete(schema, "required")
		return
	}
	schema["required"] = kept
}

func schemaDeclaresArray(schema map[string]interface{}) bool {
	switch t := schema["type"].(type) {
	case string:
		return t == "array"
	case []interface{}:
		for _, item := range t {
			if s, ok := item.(string); ok && s == "array" {
				return true
			}
		}
	}
	return false
}

func schemaDeclaresObject(schema map[string]interface{}) bool {
	switch t := schema["type"].(type) {
	case string:
		return t == "object"
	case []interface{}:
		for _, item := range t {
			if s, ok := item.(string); ok && s == "object" {
				return true
			}
		}
	}
	return false
}

// ==================== response translation: Codex SSE → OpenAI SSE ====================

// UsageInfo token usage statistics
type TokenDetails struct {
	CachedTokens int `json:"cached_tokens,omitempty"`
}

type UsageInfo struct {
	ImageInputTokens       int `json:"image_input_tokens,omitempty"`
	ImageOutputTokens      int `json:"image_output_tokens,omitempty"`
	CachedImageInputTokens int `json:"cached_image_input_tokens,omitempty"`
	PromptTokens           int `json:"prompt_tokens"`
	CompletionTokens       int `json:"completion_tokens"`
	TotalTokens            int `json:"total_tokens"`
	InputTokens            int `json:"input_tokens,omitempty"`
	OutputTokens           int `json:"output_tokens,omitempty"`
	ReasoningTokens        int `json:"reasoning_tokens,omitempty"`
	CachedTokens           int `json:"cached_tokens,omitempty"`
	// CacheWrite* are Anthropic prompt cache write tokens (cache_creation_input_tokens and its 5m/1h breakdown).
	CacheWriteTokens   int `json:"cache_write_tokens,omitempty"`
	CacheWrite5mTokens int `json:"cache_write_5m_tokens,omitempty"`
	CacheWrite1hTokens int `json:"cache_write_1h_tokens,omitempty"`
	// anthropicTotalApplied marks that InputTokens has been converted to the Anthropic total input basis, to avoid double counting.
	anthropicTotalApplied bool
	PromptTokensDetails   *TokenDetails `json:"prompt_tokens_details,omitempty"`
	InputTokensDetails    *TokenDetails `json:"input_tokens_details,omitempty"`
}

func newUsageInfo(inputTokens, outputTokens, reasoningTokens, cachedTokens int) *UsageInfo {
	usage := &UsageInfo{
		PromptTokens:     inputTokens,
		CompletionTokens: outputTokens,
		TotalTokens:      inputTokens + outputTokens,
		InputTokens:      inputTokens,
		OutputTokens:     outputTokens,
		ReasoningTokens:  reasoningTokens,
		CachedTokens:     cachedTokens,
	}
	if cachedTokens > 0 {
		details := &TokenDetails{CachedTokens: cachedTokens}
		usage.PromptTokensDetails = details
		usage.InputTokensDetails = details
	}
	return usage
}

// newContentChunk builds a text content streaming chunk
func newContentChunk(id, model string, created int64, content string) []byte {
	chunk := openAIStreamChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []streamChoice{{
			Index: 0,
			Delta: &streamDelta{Content: &content},
		}},
	}
	b, _ := json.Marshal(chunk)
	return b
}

// newReasoningChunk builds a reasoning content streaming chunk.
// Fills in both reasoning and reasoning_content, compatible with both the OpenAI and DeepSeek client styles.
func newReasoningChunk(id, model string, created int64, reasoning string) []byte {
	chunk := openAIStreamChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []streamChoice{{
			Index: 0,
			Delta: &streamDelta{
				Reasoning:        &reasoning,
				ReasoningContent: &reasoning,
			},
		}},
	}
	b, _ := json.Marshal(chunk)
	return b
}

func isCodexToolCallItemType(itemType string) bool {
	switch itemType {
	case "function_call", "custom_tool_call":
		return true
	default:
		return false
	}
}

func isCodexToolInputDeltaEvent(eventType string) bool {
	switch eventType {
	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		return true
	default:
		return false
	}
}

func isCodexToolInputDoneEvent(eventType string) bool {
	switch eventType {
	case "response.function_call_arguments.done", "response.custom_tool_call_input.done":
		return true
	default:
		return false
	}
}

// newToolCallAnnouncementChunk builds the first chunk of a tool call (containing id, type, function.name)
func newToolCallAnnouncementChunk(id, model string, created int64, tcIndex int, callID, funcName string) []byte {
	chunk := openAIStreamChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []streamChoice{{
			Index: 0,
			Delta: &streamDelta{
				Role: "assistant",
				ToolCalls: []toolCallDelta{{
					Index: tcIndex,
					ID:    callID,
					Type:  "function",
					Function: &toolCallFuncDelta{
						Name:      funcName,
						Arguments: "",
					},
				}},
			},
		}},
	}
	b, _ := json.Marshal(chunk)
	return b
}

// newToolCallDeltaChunk builds a tool call argument delta chunk
func newToolCallDeltaChunk(id, model string, created int64, tcIndex int, argsDelta string) []byte {
	chunk := openAIStreamChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []streamChoice{{
			Index: 0,
			Delta: &streamDelta{
				ToolCalls: []toolCallDelta{{
					Index:    tcIndex,
					Function: &toolCallFuncDelta{Arguments: argsDelta},
				}},
			},
		}},
	}
	b, _ := json.Marshal(chunk)
	return b
}

// newFinalChunk builds the final streaming chunk (containing finish_reason and optional usage).
//
// delta must explicitly carry an empty object: the OpenAI official final chunk looks like
// {"index":0,"delta":{},"finish_reason":"stop"}, and strictly deserializing clients
// (the Rust/serde family) treat delta as a required field; if it is missing they report
// "missing field `delta`" directly and the whole conversation fails.
func newFinalChunk(id, model string, created int64, finishReason string, usage *UsageInfo) []byte {
	return newFinalChunkWithTier(id, model, created, finishReason, usage, "")
}

// newFinalChunkWithTier attaches the upstream's actual service_tier to the final chunk (omitted if empty).
func newFinalChunkWithTier(id, model string, created int64, finishReason string, usage *UsageInfo, serviceTier string) []byte {
	chunk := openAIStreamChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []streamChoice{{
			Index:        0,
			Delta:        &streamDelta{},
			FinishReason: &finishReason,
		}},
		Usage:       usage,
		ServiceTier: strings.TrimSpace(serviceTier),
	}
	b, _ := json.Marshal(chunk)
	return b
}

// newErrorResponse builds an error response
func newErrorResponse(message string, details ...json.RawMessage) []byte {
	resp := openAIErrorResponse{}
	resp.Error.Message = message
	resp.Error.Type = "upstream_error"
	if len(details) > 0 && len(details[0]) > 0 {
		resp.Error.Details = details[0]
	}
	b, _ := json.Marshal(resp)
	return b
}

// ChatStreamTranslation keeps terminal success and terminal failure distinct.
// Both end the upstream Responses stream, but only a successful completion may
// be followed by Chat Completions' [DONE] sentinel. Treating a response.failed
// as the old undifferentiated done=true result makes a failed generation look
// like a cleanly completed Chat stream to downstream clients.
type ChatStreamTranslation struct {
	Chunk    []byte
	Terminal bool
	Failed   bool
}

func newChatStreamTranslation(eventType string, chunk []byte, terminal bool) ChatStreamTranslation {
	return ChatStreamTranslation{
		Chunk:    chunk,
		Terminal: terminal,
		Failed:   terminal && eventType == "response.failed",
	}
}

// TranslateStreamChunkResult is the terminal-aware form of
// TranslateStreamChunk. Existing callers that only need the historical
// (chunk, done) pair can keep using TranslateStreamChunk.
func TranslateStreamChunkResult(eventData []byte, model string, chunkID string, created int64) ChatStreamTranslation {
	eventType := gjson.GetBytes(eventData, "type").String()
	chunk, terminal := TranslateStreamChunk(eventData, model, chunkID, created)
	return newChatStreamTranslation(eventType, chunk, terminal)
}

// TranslateStreamChunk translates a Codex SSE data chunk into OpenAI Chat Completions streaming format (stateless)
func TranslateStreamChunk(eventData []byte, model string, chunkID string, created int64) ([]byte, bool) {
	eventType := gjson.GetBytes(eventData, "type").String()

	switch eventType {
	case "response.output_text.delta":
		delta := gjson.GetBytes(eventData, "delta").String()
		return newContentChunk(chunkID, model, created, delta), false

	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		delta := gjson.GetBytes(eventData, "delta").String()
		return newReasoningChunk(chunkID, model, created, delta), false

	case "response.custom_tool_call_input.delta", "response.custom_tool_call_input.done":
		return nil, false

	case "response.completed":
		usage := extractUsage(eventData)
		return newFinalChunkWithTier(chunkID, model, created, "stop", usage, gjson.GetBytes(eventData, "response.service_tier").String()), true

	// Normal terminal state for max_output_tokens truncation: on the Chat side this corresponds to finish_reason=length.
	case "response.incomplete":
		usage := extractUsage(eventData)
		reason := gjson.GetBytes(eventData, "response.incomplete_details.reason").String()
		return newFinalChunkWithTier(chunkID, model, created, responsesIncompleteFinishReason(eventType, reason), usage, gjson.GetBytes(eventData, "response.service_tier").String()), true

	case "response.failed":
		errMsg := gjson.GetBytes(eventData, "response.error.message").String()
		if errMsg == "" {
			errMsg = "Codex upstream error"
		}
		var details json.RawMessage
		if raw := gjson.GetBytes(eventData, "response.error.details"); raw.Exists() && raw.IsObject() {
			details = json.RawMessage(raw.Raw)
		}
		return newErrorResponse(errMsg, details), true

	case "response.content_part.done", "response.output_item.done",
		"response.created", "response.in_progress",
		"response.output_item.added", "response.content_part.added",
		"response.reasoning_summary_text.done",
		"response.reasoning.encrypted_content.delta", "response.reasoning.encrypted_content.done",
		"response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		return nil, false

	default:
		if delta := gjson.GetBytes(eventData, "delta"); delta.Exists() && delta.Type == gjson.String {
			return newContentChunk(chunkID, model, created, delta.String()), false
		}
		return nil, false
	}
}

// ==================== stateful streaming converter (supports Function Calling) ====================

// ToolCallResult represents one complete tool call result
type ToolCallResult struct {
	ID        string
	Name      string
	Arguments string
	Type      string
	Namespace string
}

// StreamTranslator is a stateful streaming response translator that tracks the function_call index mapping
type StreamTranslator struct {
	Model                 string
	ChunkID               string
	Created               int64
	HasToolCalls          bool
	toolCallMap           map[string]int // Codex item.id/call_id → OpenAI tool_calls index
	toolCallOutputIndexes map[int]int    // Responses output_index → OpenAI tool_calls index
	toolCallTypes         map[int]string
	toolCallNames         map[int]string
	toolCallArguments     map[int]string
	customToolInputs      map[int]*strings.Builder
	toolCallFinalized     map[int]bool
	invalidToolArguments  error
	nextIdx               int
	// toolNameRestore maps upstream name → the client's original name (codex_tool_names.go); nil means no rewriting.
	toolNameRestore map[string]string
}

// SetToolNameRestore registers the tool name restore mapping; it can be passed nil when the request side did not rewrite any name.
func (st *StreamTranslator) SetToolNameRestore(restore map[string]string) {
	if st == nil {
		return
	}
	st.toolNameRestore = restore
}

// NewStreamTranslator creates a stream translator instance
func NewStreamTranslator(chunkID, model string, created int64) *StreamTranslator {
	return &StreamTranslator{
		Model:                 model,
		ChunkID:               chunkID,
		Created:               created,
		toolCallMap:           make(map[string]int),
		toolCallOutputIndexes: make(map[int]int),
		toolCallTypes:         make(map[int]string),
		toolCallNames:         make(map[int]string),
		toolCallArguments:     make(map[int]string),
		customToolInputs:      make(map[int]*strings.Builder),
		toolCallFinalized:     make(map[int]bool),
	}
}

// Translate converts a single Codex SSE event to the OpenAI Chat Completions streaming format
func (st *StreamTranslator) Translate(eventData []byte) ([]byte, bool) {
	return st.TranslateParsed(gjson.ParseBytes(eventData))
}

// TranslateParsedResult exposes whether a terminal translation represents a
// successful completion or a failure. This lets the HTTP handler terminate a
// failed stream without appending the success-only [DONE] sentinel.
func (st *StreamTranslator) TranslateParsedResult(parsed gjson.Result) ChatStreamTranslation {
	chunk, terminal := st.TranslateParsed(parsed)
	result := newChatStreamTranslation(parsed.Get("type").String(), chunk, terminal)
	if terminal && st.invalidToolArguments != nil {
		result.Failed = true
	}
	return result
}

// ToolArgumentsError reports an upstream protocol failure detected while
// rebuilding a streamed ordinary function call. The handler uses this to make
// the failure retryable before any private buffered attempt is committed.
func (st *StreamTranslator) ToolArgumentsError() error {
	if st == nil {
		return nil
	}
	return st.invalidToolArguments
}

func (st *StreamTranslator) toolCallIndex(parsed gjson.Result) (int, bool) {
	itemID := parsed.Get("item_id").String()
	if itemID == "" {
		itemID = parsed.Get("call_id").String()
	}
	if itemID == "" {
		itemID = parsed.Get("item.id").String()
	}
	if itemID == "" {
		itemID = parsed.Get("item.call_id").String()
	}
	if itemID != "" {
		if idx, ok := st.toolCallMap[itemID]; ok {
			return idx, true
		}
	}
	if outputIndex := parsed.Get("output_index"); outputIndex.Exists() {
		idx, ok := st.toolCallOutputIndexes[int(outputIndex.Int())]
		return idx, ok
	}
	return 0, false
}

func (st *StreamTranslator) failToolArguments(idx int, reason string) ([]byte, bool) {
	if st.invalidToolArguments == nil {
		name := strings.TrimSpace(st.toolCallNames[idx])
		if name == "" {
			name = "unknown"
		}
		if st.toolCallTypes[idx] == "custom_tool_call" {
			st.invalidToolArguments = fmt.Errorf("upstream custom tool call %q input %s", name, reason)
		} else {
			st.invalidToolArguments = fmt.Errorf("upstream function call %q arguments %s", name, reason)
		}
	}
	return newErrorResponse(st.invalidToolArguments.Error()), true
}

// finalizeOrdinaryToolArguments reconciles streamed deltas with the canonical
// arguments carried by the done item. A canonical suffix may be emitted when a
// provider omitted the final delta; divergent or invalid JSON fails the attempt
// instead of producing a completed poisoned tool call.
func (st *StreamTranslator) finalizeOrdinaryToolArguments(idx int, finalArguments string) ([]byte, bool) {
	current := st.toolCallArguments[idx]
	candidate := finalArguments
	if strings.TrimSpace(candidate) == "" && current != "" {
		candidate = current
	}
	normalized, valid := normalizeOrdinaryFunctionCallArguments(candidate)
	if !valid {
		return st.failToolArguments(idx, "contain invalid JSON")
	}
	if current == normalized {
		st.toolCallFinalized[idx] = true
		return nil, false
	}
	if !strings.HasPrefix(normalized, current) {
		return st.failToolArguments(idx, "do not match the streamed deltas")
	}
	st.toolCallArguments[idx] = normalized
	st.toolCallFinalized[idx] = true
	return newToolCallDeltaChunk(st.ChunkID, st.Model, st.Created, idx, normalized[len(current):]), false
}

func (st *StreamTranslator) validateTerminalFunctionCalls(parsed gjson.Result) {
	if st.invalidToolArguments != nil {
		return
	}
	output := parsed.Get("response.output")
	if output.IsArray() {
		output.ForEach(func(_, item gjson.Result) bool {
			if item.Get("type").String() != "function_call" {
				return true
			}
			if _, valid := normalizeOrdinaryFunctionCallArguments(item.Get("arguments").String()); !valid {
				name := strings.TrimSpace(item.Get("name").String())
				if name == "" {
					name = "unknown"
				}
				st.invalidToolArguments = fmt.Errorf("upstream function call %q arguments contain invalid JSON", name)
				return false
			}
			return true
		})
	}
	if st.invalidToolArguments != nil {
		return
	}
	for idx, itemType := range st.toolCallTypes {
		if itemType != "function_call" || st.toolCallFinalized[idx] {
			continue
		}
		if _, valid := normalizeOrdinaryFunctionCallArguments(st.toolCallArguments[idx]); !valid {
			_, _ = st.failToolArguments(idx, "contain invalid JSON")
			return
		}
	}
}

// TranslateParsed converts a parsed Codex SSE event to the OpenAI Chat Completions streaming format.
func (st *StreamTranslator) TranslateParsed(parsed gjson.Result) ([]byte, bool) {
	eventType := parsed.Get("type").String()

	switch eventType {
	case "response.output_text.delta":
		delta := parsed.Get("delta").String()
		return newContentChunk(st.ChunkID, st.Model, st.Created, delta), false

	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		delta := parsed.Get("delta").String()
		return newReasoningChunk(st.ChunkID, st.Model, st.Created, delta), false

	case "response.output_item.added":
		itemType := parsed.Get("item.type").String()
		if !isCodexToolCallItemType(itemType) {
			return nil, false
		}
		callID := parsed.Get("item.call_id").String()
		if callID == "" {
			callID = parsed.Get("item.id").String()
		}
		name := restoreCodexToolName(st.toolNameRestore, parsed.Get("item.name").String())
		itemID := parsed.Get("item.id").String()
		if itemID == "" {
			itemID = callID
		}

		tcIdx := st.nextIdx
		st.toolCallMap[itemID] = tcIdx
		if callID != "" && callID != itemID {
			st.toolCallMap[callID] = tcIdx
		}
		if outputIndex := parsed.Get("output_index"); outputIndex.Exists() {
			st.toolCallOutputIndexes[int(outputIndex.Int())] = tcIdx
		}
		st.nextIdx++
		st.HasToolCalls = true
		st.toolCallTypes[tcIdx] = itemType
		st.toolCallNames[tcIdx] = name

		if itemType == "custom_tool_call" {
			return newCustomToolChunk(st.ChunkID, st.Model, st.Created, tcIdx, callID, name, "", parsed.Get("item.namespace").String()), false
		}
		return newToolCallAnnouncementChunk(st.ChunkID, st.Model, st.Created, tcIdx, callID, name), false

	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		tcIdx, ok := st.toolCallIndex(parsed)
		if !ok {
			return nil, false
		}
		delta := parsed.Get("delta").String()
		if st.toolCallTypes[tcIdx] == "custom_tool_call" {
			return st.appendCustomToolInput(tcIdx, delta)
		}
		if eventType == "response.function_call_arguments.delta" && st.toolCallTypes[tcIdx] == "function_call" {
			if st.toolCallFinalized[tcIdx] {
				return st.failToolArguments(tcIdx, "continued after the done event")
			}
			st.toolCallArguments[tcIdx] += delta
		}
		return newToolCallDeltaChunk(st.ChunkID, st.Model, st.Created, tcIdx, delta), false

	case "response.function_call_arguments.done":
		tcIdx, ok := st.toolCallIndex(parsed)
		if !ok || st.toolCallTypes[tcIdx] != "function_call" {
			return nil, false
		}
		return st.finalizeOrdinaryToolArguments(tcIdx, parsed.Get("arguments").String())

	case "response.custom_tool_call_input.done":
		tcIdx, ok := st.toolCallIndex(parsed)
		if !ok || st.toolCallTypes[tcIdx] != "custom_tool_call" {
			return nil, false
		}
		return st.finalizeCustomToolInput(tcIdx, parsed.Get("input"))

	case "response.output_item.done":
		if parsed.Get("item.type").String() == "custom_tool_call" {
			if idx, ok := st.toolCallIndex(parsed); ok {
				return st.finalizeCustomToolInput(idx, parsed.Get("item.input"))
			}
		}
		if parsed.Get("item.type").String() != "function_call" {
			return nil, false
		}
		tcIdx, ok := st.toolCallIndex(parsed)
		if !ok {
			return nil, false
		}
		return st.finalizeOrdinaryToolArguments(tcIdx, parsed.Get("item.arguments").String())

	case "response.completed", "response.incomplete":
		st.validateTerminalFunctionCalls(parsed)
		if st.invalidToolArguments != nil {
			return newErrorResponse(st.invalidToolArguments.Error()), true
		}
		usage := extractUsageFromResult(parsed.Get("response.usage"))
		finishReason := "stop"
		if st.HasToolCalls {
			finishReason = "tool_calls"
		}
		// The truncated state overrides the derived value: stop / tool_calls would present partial output as a normal finish.
		if override := responsesIncompleteFinishReason(eventType, parsed.Get("response.incomplete_details.reason").String()); override != "" {
			finishReason = override
		}
		return newFinalChunkWithTier(st.ChunkID, st.Model, st.Created, finishReason, usage, parsed.Get("response.service_tier").String()), true

	case "response.failed":
		errMsg := parsed.Get("response.error.message").String()
		if errMsg == "" {
			errMsg = "Codex upstream error"
		}
		var details json.RawMessage
		if raw := parsed.Get("response.error.details"); raw.Exists() && raw.IsObject() {
			details = json.RawMessage(raw.Raw)
		}
		return newErrorResponse(errMsg, details), true

	case "response.content_part.done",
		"response.created", "response.in_progress",
		"response.content_part.added",
		"response.reasoning_summary_text.done",
		"response.reasoning.encrypted_content.delta", "response.reasoning.encrypted_content.done",
		"response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		return nil, false

	default:
		if delta := parsed.Get("delta"); delta.Exists() && delta.Type == gjson.String {
			return newContentChunk(st.ChunkID, st.Model, st.Created, delta.String()), false
		}
		return nil, false
	}
}

// ==================== Non-streaming response translation ====================

// TranslateCompactResponse converts a Codex non-streaming response to the OpenAI format
func TranslateCompactResponse(responseData []byte, model string, id string) []byte {
	var outputText, reasoningText string
	output := gjson.GetBytes(responseData, "output")
	if output.IsArray() {
		output.ForEach(func(_, item gjson.Result) bool {
			switch item.Get("type").String() {
			case "message":
				content := item.Get("content")
				if content.IsArray() {
					content.ForEach(func(_, part gjson.Result) bool {
						if part.Get("type").String() == "output_text" {
							outputText += part.Get("text").String()
						}
						return true
					})
				}
			case "reasoning":
				// Codex puts the thinking process in response.output as a reasoning item,
				// each element under the content/summary array has the form {type, text}.
				summary := item.Get("summary")
				if summary.IsArray() {
					summary.ForEach(func(_, part gjson.Result) bool {
						reasoningText += part.Get("text").String()
						return true
					})
				}
				content := item.Get("content")
				if content.IsArray() {
					content.ForEach(func(_, part gjson.Result) bool {
						reasoningText += part.Get("text").String()
						return true
					})
				}
			}
			return true
		})
	}

	usage := extractUsage(responseData)

	msg := compactMessage{
		Role:    "assistant",
		Content: &outputText,
	}
	if reasoningText != "" {
		r := reasoningText
		msg.Reasoning = &r
		msg.ReasoningContent = &r
	}

	resp := openAICompactResponse{
		ID:     id,
		Object: "chat.completion",
		Model:  model,
		Choices: []compactChoice{{
			Index:        0,
			Message:      msg,
			FinishReason: "stop",
		}},
		Usage: usage,
	}
	b, _ := json.Marshal(resp)
	return b
}

// BuildCompactResponse builds the complete non-streaming response (called by handler.go, replacing the inline sjson)
// When there are toolCalls and content is empty, content is output as JSON null
// reasoning is the concatenated thinking text; when it is an empty string, the reasoning / reasoning_content fields are omitted.
func BuildCompactResponse(id, model string, created int64, content, reasoning string, toolCalls []ToolCallResult, usage *UsageInfo) []byte {
	return BuildCompactResponseWithFinishReason(id, model, created, content, reasoning, toolCalls, usage, "")
}

// BuildCompactResponseWithFinishReason is the same, and additionally lets the caller override finish_reason.
// When upstream truncates at max_output_tokens, the terminal state is response.incomplete, and derived values such as stop /
// tool_calls would present the truncated response as a normal finish, so it must be overridden to length. An empty string means no override.
func BuildCompactResponseWithFinishReason(id, model string, created int64, content, reasoning string, toolCalls []ToolCallResult, usage *UsageInfo, finishReasonOverride string) []byte {
	return BuildCompactChatResponse(id, model, created, content, reasoning, toolCalls, usage, finishReasonOverride, "")
}

// BuildCompactChatResponse builds on BuildCompactResponseWithFinishReason and also includes the upstream
// actual service_tier (omitted when empty).
func BuildCompactChatResponse(id, model string, created int64, content, reasoning string, toolCalls []ToolCallResult, usage *UsageInfo, finishReasonOverride string, serviceTier string) []byte {
	finishReason := "stop"
	msg := compactMessage{
		Role:    "assistant",
		Content: &content,
	}
	if reasoning != "" {
		r := reasoning
		msg.Reasoning = &r
		msg.ReasoningContent = &r
	}

	if len(toolCalls) > 0 {
		finishReason = "tool_calls"
		if content == "" {
			msg.Content = nil // JSON null
		}
		msg.ToolCalls = make([]compactToolCallOut, len(toolCalls))
		for i, tc := range toolCalls {
			msg.ToolCalls[i] = compactToolCallOut{
				ID:   tc.ID,
				Type: "function",
			}
			msg.ToolCalls[i].Namespace = tc.Namespace
			if tc.Type == "custom_tool_call" || tc.Type == "custom" {
				msg.ToolCalls[i].Type = "custom"
				msg.ToolCalls[i].Custom = &customToolCallData{Name: tc.Name, Input: tc.Arguments}
			} else {
				msg.ToolCalls[i].Function = &toolCallFuncDelta{Name: tc.Name, Arguments: tc.Arguments}
			}
		}
	}
	if finishReasonOverride != "" {
		finishReason = finishReasonOverride
	}

	resp := openAICompactResponse{
		ID:      id,
		Object:  "chat.completion",
		Created: created,
		Model:   model,
		Choices: []compactChoice{{
			Index:        0,
			Message:      msg,
			FinishReason: finishReason,
		}},
		Usage:       usage,
		ServiceTier: strings.TrimSpace(serviceTier),
	}
	b, _ := json.Marshal(resp)
	return b
}

// ==================== Common utility functions ====================

// extractUsage extracts usage from the response.completed event
func extractUsage(eventData []byte) *UsageInfo {
	return extractUsageFromResult(gjson.GetBytes(eventData, "response.usage"))
}

// extractUsageFromResult extracts usage from an already parsed gjson.Result (avoids parsing twice)
func extractUsageFromResult(usage gjson.Result) *UsageInfo {
	if !usage.Exists() {
		return nil
	}
	inputTokens := int(usage.Get("input_tokens").Int())
	outputTokens := int(usage.Get("output_tokens").Int())
	reasoningTokens := int(usage.Get("output_tokens_details.reasoning_tokens").Int())
	cachedTokens := int(usage.Get("input_tokens_details.cached_tokens").Int())
	result := newUsageInfo(inputTokens, outputTokens, reasoningTokens, cachedTokens)
	result.ImageInputTokens = min(max(0, int(usage.Get("input_tokens_details.image_tokens").Int())), max(0, inputTokens))
	result.ImageOutputTokens = min(max(0, int(usage.Get("output_tokens_details.image_tokens").Int())), max(0, outputTokens))
	result.CachedImageInputTokens = min(max(0, int(usage.Get("input_tokens_details.cached_tokens_details.image_tokens").Int())), min(result.ImageInputTokens, max(0, cachedTokens)))
	return result
}

// ExtractToolCallsFromOutputValidated extracts completed tool calls and rejects
// malformed ordinary function arguments. Custom tool input remains free-form.
func ExtractToolCallsFromOutputValidated(eventData []byte) ([]ToolCallResult, error) {
	var toolCalls []ToolCallResult
	output := gjson.GetBytes(eventData, "response.output")
	if !output.IsArray() {
		return nil, nil
	}
	var validationErr error
	output.ForEach(func(_, item gjson.Result) bool {
		itemType := item.Get("type").String()
		if isCodexToolCallItemType(itemType) {
			callID := item.Get("call_id").String()
			if callID == "" {
				callID = item.Get("id").String()
			}
			arguments := item.Get("arguments").String()
			if itemType == "custom_tool_call" {
				arguments = item.Get("input").String()
			} else {
				normalized, valid := normalizeOrdinaryFunctionCallArguments(arguments)
				if !valid {
					name := strings.TrimSpace(item.Get("name").String())
					if name == "" {
						name = "unknown"
					}
					validationErr = fmt.Errorf("upstream function call %q arguments contain invalid JSON", name)
					return false
				}
				arguments = normalized
			}
			toolCalls = append(toolCalls, ToolCallResult{
				ID:        callID,
				Type:      itemType,
				Namespace: item.Get("namespace").String(),
				Name:      item.Get("name").String(),
				Arguments: arguments,
			})
		}
		return true
	})
	if validationErr != nil {
		return nil, validationErr
	}
	return toolCalls, nil
}

// ExtractToolCallsFromOutput preserves the historical convenience API. New
// response paths should use ExtractToolCallsFromOutputValidated so malformed
// upstream output becomes an explicit failed attempt rather than disappearing.
func ExtractToolCallsFromOutput(eventData []byte) []ToolCallResult {
	toolCalls, _ := ExtractToolCallsFromOutputValidated(eventData)
	return toolCalls
}

func malformedToolArgumentsFailurePayload(err error) []byte {
	message := "upstream function call arguments contain invalid JSON"
	if err != nil && strings.TrimSpace(err.Error()) != "" {
		message = err.Error()
	}
	payload := map[string]any{
		"type": "response.failed",
		"response": map[string]any{
			"created_at": time.Now().Unix(),
			"status":     "failed",
			"error": map[string]any{
				"code":        "bad_gateway",
				"type":        "upstream_protocol_error",
				"status_code": http.StatusBadGateway,
				"message":     message,
			},
		},
	}
	encoded, _ := json.Marshal(payload)
	return encoded
}
