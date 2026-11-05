package proxy

import (
	"log"
	"strings"
	"sync"

	"github.com/tidwall/gjson"
)

// The upstream gates Responses Lite per model: the use_responses_lite field of the model manifest declares which
// models use the lite channel (measured: the whole gpt-5.6 family and codex-auto-review are true, gpt-5.5/5.4
// are false). A model that does not support it, if sent a lite signal, always gets a 400 unsupported_value (param=model,
// "This model is not supported when using X-OpenAI-Internal-Codex-Responses-Lite").
// The downstream client decides whether to send the signal based on "the model it itself requested", but the gateway's model mapping/payload rules
// may rewrite the model into another one, so this maintains a slug→capability table and strips the doomed combination before sending.
//
// The builtin seed comes from the measured 2026-08 upstream manifest; whenever the manifest passes through or syncs via the gateway,
// RecordResponsesLiteSupportFromManifest learns the true-value override, so the seed can self-heal once it goes stale.
var builtinResponsesLiteSupport = map[string]bool{
	"gpt-5.6-sol":       true,
	"gpt-5.6-terra":     true,
	"gpt-5.6-luna":      true,
	"codex-auto-review": true,
	"gpt-5.5":           false,
	"gpt-5.4":           false,
	"gpt-5.4-mini":      false,
}

var learnedResponsesLiteSupport = struct {
	sync.RWMutex
	bySlug map[string]bool
}{bySlug: make(map[string]bool)}

// RecordResponsesLiteSupportFromManifest learns, from the upstream model manifest, each model's
// use_responses_lite capability. The manifest is the upstream source of truth and overrides the builtin seed; entries whose field is missing or non-boolean
// entries are skipped (they stay "unknown" and are not touched).
func RecordResponsesLiteSupportFromManifest(manifest []byte) {
	if len(manifest) == 0 {
		return
	}
	items := gjson.GetBytes(manifest, "models")
	if !items.IsArray() {
		return
	}
	type liteEntry struct {
		slug string
		lite bool
	}
	entries := make([]liteEntry, 0, 8)
	items.ForEach(func(_, item gjson.Result) bool {
		slug := strings.ToLower(strings.TrimSpace(item.Get("slug").String()))
		if slug == "" {
			return true
		}
		flag := item.Get("use_responses_lite")
		if flag.Type != gjson.True && flag.Type != gjson.False {
			return true
		}
		entries = append(entries, liteEntry{slug: slug, lite: flag.Bool()})
		return true
	})
	if len(entries) == 0 {
		return
	}
	learnedResponsesLiteSupport.Lock()
	for _, entry := range entries {
		learnedResponsesLiteSupport.bySlug[entry.slug] = entry.lite
	}
	learnedResponsesLiteSupport.Unlock()
}

// responsesLiteSupportForModel returns whether the model supports Responses Lite.
// known=false means neither table has the model (a new model or alias); the caller should keep passing it through untouched.
func responsesLiteSupportForModel(model string) (supports bool, known bool) {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return false, false
	}
	learnedResponsesLiteSupport.RLock()
	supports, known = learnedResponsesLiteSupport.bySlug[model]
	learnedResponsesLiteSupport.RUnlock()
	if known {
		return supports, true
	}
	supports, known = builtinResponsesLiteSupport[model]
	return supports, known
}

// gateResponsesLiteForModel, after the model is finalised, reconciles the downstream lite signal: for models known not to support lite, the
// signal is stripped (it would always 400 → becomes runnable); supported or unknown models are passed through unchanged.
func gateResponsesLiteForModel(requested bool, requestBody []byte) bool {
	if !requested {
		return false
	}
	model := strings.TrimSpace(gjson.GetBytes(requestBody, "model").String())
	if supports, known := responsesLiteSupportForModel(model); known && !supports {
		log.Printf("[Lite] model %s does not support Responses Lite; the downstream lite signal was stripped (model mapping/rule rewrite or downstream sent it by mistake)", model)
		return false
	}
	return true
}
