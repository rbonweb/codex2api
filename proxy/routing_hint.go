package proxy

import (
	"net/http"
	"os"
	"strings"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

// codexRoutingHintHeader is the request header the official Codex client uses to declare routing intent to the upstream,
// its value looks like model=<slug> or model=<slug>;tier=priority|flex.
const codexRoutingHintHeader = "x-codex-routing-hint"

// CodexRoutingHintDisabled reports whether the routing hint outbound has been turned off by an environment variable.
// When CODEX_DISABLE_ROUTING_HINT=1 (or true), the header is no longer synthesized; inbound spoofing is still stripped.
func CodexRoutingHintDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CODEX_DISABLE_ROUTING_HINT"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// buildCodexRoutingHint synthesizes the hint value from the final outbound body (after model mapping and tier sanitizing).
// It returns empty (the whole header is not sent) when model is missing, contains a separator, or the assembled value contains control characters.
// tier allows priority (fast is normalized), ultrafast and flex; default is an explicit standard-route sentinel,
// and like missing, auto, scale or unknown values, it degrades to the model-only form.
func buildCodexRoutingHint(requestBody []byte) string {
	results := gjson.GetManyBytes(requestBody, "model", "service_tier")
	model := strings.TrimSpace(results[0].String())
	if model == "" || strings.ContainsAny(model, ";=") {
		return ""
	}
	hint := "model=" + model
	tier := strings.ToLower(strings.TrimSpace(results[1].String()))
	if tier == "fast" {
		tier = "priority"
	}
	if tier == "priority" || tier == "flex" || tier == "ultrafast" {
		hint += ";tier=" + tier
	}
	if !validHTTPHeaderValue(hint) {
		return ""
	}
	return hint
}

// ApplyCodexRoutingHint first strips any existing copy of this header in any letter case (owned by the gateway; downstream and
// account custom headers passed in are never trusted), then synthesizes it from the final outbound body only for official Codex accounts.
func ApplyCodexRoutingHint(headers http.Header, account *auth.Account, requestBody []byte) {
	if headers == nil {
		return
	}
	deleteHeaderCaseInsensitive(headers, codexRoutingHintHeader)
	if account == nil || CodexRoutingHintDisabled() {
		return
	}
	if hint := buildCodexRoutingHint(requestBody); hint != "" {
		headers.Set(codexRoutingHintHeader, hint)
	}
}

// deleteHeaderCaseInsensitive deletes a header with the same name in any letter case
// (http.Header.Del only deletes the canonical key, while a hand-built map may carry lower-case keys).
func deleteHeaderCaseInsensitive(headers http.Header, name string) {
	for key := range headers {
		if strings.EqualFold(key, name) {
			delete(headers, key)
		}
	}
}
