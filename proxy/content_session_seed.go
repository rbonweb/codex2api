package proxy

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/tidwall/gjson"
)

// deriveContentSessionSeed derives a stable session seed from the request body (modeled on sub2api's
// deriveOpenAIContentSessionSeed). It only takes fields that stay unchanged throughout the conversation: model, instructions,
// the system/developer prefix at the start of the conversation, the tools definitions, and the first user message.
//
// Each text field that goes into the hash first passes through normalizeClientScaffolding, which removes the scaffolding
// that the client injects every turn (<system-reminder> environment blocks, git attribution, "powered by the model", etc.)
// is excluded. Two reasons:
//
//  1. These blocks change every turn (working directory, git status, file list); if left in the hash, they would move the same conversation
//     to another account by mistake, losing account stickiness, and the upstream prompt cache would be invalidated along with it;
//  2. The identity key should be derived from "the content that is actually sent upstream". The scaffolding is already stripped on the way out; leaving it in the identity key
//     would only make the identity and the payload give two different answers for the same conversation.
//
// Dynamic system messages after the first non-system/developer item are likewise excluded from the hash, so that when the client injects
// time, environment, or status hints every turn, the same session does not drift by mistake. Real content still takes part, so different conversations
// still do not interfere with each other.
//
// Purpose: when the client carries no explicit session identifier (Session_id/Conversation_id/Idempotency-Key/
// prompt_cache_key), multi-turn requests of "the same conversation" converge on the same local account stickiness key,
// and different conversations do not interfere with each other. In deployments where a single API Key is shared by multiple terminal users, the stickiness granularity moves from "the whole Key
// piles onto one account" to "each conversation is pinned to its own account". This seed only takes part in local routing
// (account stickiness affinityKey); in the default isolation mode it is not sent upstream.
//
// Returns "" when it cannot be derived (the caller falls back to deriving from the API Key):
//   - the request has previous_response_id: the client's continuation input, which does not resend history, changes every turn,
//     so a content seed would drift every turn, causing account switches and breaking the continuation; a stable per-Key fallback is kept;
//   - there is no first user message to anchor on: the seed quality is too low, and a per-Key fallback is more predictable.
func deriveContentSessionSeed(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	if gjson.GetBytes(body, "previous_response_id").String() != "" {
		return ""
	}

	h := sha256.New()
	writeField := func(tag, val string) {
		if val == "" {
			return
		}
		h.Write([]byte(tag))
		h.Write([]byte{0})
		h.Write([]byte(val))
		h.Write([]byte{0})
	}

	writeField("model", gjson.GetBytes(body, "model").String())
	writeField("instructions", normalizeClientScaffolding(gjson.GetBytes(body, "instructions").String()))
	if tools := gjson.GetBytes(body, "tools"); tools.IsArray() && tools.Raw != "[]" {
		writeField("tools", tools.Raw)
	}

	firstUserCaptured := false
	scanItems := func(items gjson.Result) {
		systemPrefixOpen := true
		items.ForEach(func(_, item gjson.Result) bool {
			role := item.Get("role").String()
			switch role {
			case "system", "developer":
				if systemPrefixOpen {
					writeField("system", normalizeClientScaffolding(item.Get("content").Raw))
				}
			case "user":
				systemPrefixOpen = false
				if !firstUserCaptured {
					writeField("first_user", normalizeClientScaffolding(item.Get("content").Raw))
					firstUserCaptured = true
				}
			default:
				systemPrefixOpen = false
			}
			return true
		})
	}

	// The Responses format (input as a string or array) takes priority; fall back to Chat Completions messages.
	if input := gjson.GetBytes(body, "input"); input.Exists() {
		if input.Type == gjson.String {
			if s := normalizeClientScaffolding(input.String()); s != "" {
				writeField("first_user", s)
				firstUserCaptured = true
			}
		} else if input.IsArray() {
			scanItems(input)
		}
	} else if msgs := gjson.GetBytes(body, "messages"); msgs.IsArray() {
		scanItems(msgs)
	}

	if !firstUserCaptured {
		return ""
	}
	sum := h.Sum(nil)
	return "content-" + hex.EncodeToString(sum[:16])
}
