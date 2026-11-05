package proxy

import (
	"bytes"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"

	"github.com/codex2api/database"
)

// The Codex client's "Ultra" tier is not a reasoning.effort value on the wire: the client translates it into
// effort=xhigh and appends a <multi_agent_mode> developer message to input to enable proactive
// multi-agent delegation; when switching back to another tier it appends a "no longer applies" message to turn it off (issue #669,
// verified by packet capture). Therefore Ultra can only be determined from the last multi_agent_mode message in the request body.
//
// Continuation frames within the same turn (with previous_response_id and input holding only tool output) carry no history,
// so they cannot see this message and inherit the first frame's result for this turn by turn_id.
const (
	ultraModeMarkerTag  = "<multi_agent_mode>"
	ultraModeActiveText = "Proactive multi-agent delegation is active"

	requestUltraModeContextKey = "codex2api.request.ultra_mode"

	ultraModeTurnTTL        = 6 * time.Hour
	ultraModeTurnMaxEntries = 20000
)

type ultraModeState uint8

const (
	ultraModeUnknown ultraModeState = iota
	ultraModeOff
	ultraModeOn
)

// detectUltraModeFromBody only looks at developer messages at the top level of input, and uses the last
// multi_agent_mode message as authoritative; with no marker at all it returns unknown.
func detectUltraModeFromBody(body []byte) ultraModeState {
	if len(body) == 0 || !bytes.Contains(body, []byte("multi_agent_mode")) {
		return ultraModeUnknown
	}
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return ultraModeUnknown
	}
	state := ultraModeUnknown
	inspect := func(text string) {
		if !strings.Contains(text, ultraModeMarkerTag) {
			return
		}
		if strings.Contains(text, ultraModeActiveText) {
			state = ultraModeOn
		} else {
			state = ultraModeOff
		}
	}
	input.ForEach(func(_, item gjson.Result) bool {
		if !item.IsObject() || item.Get("role").String() != "developer" {
			return true
		}
		content := item.Get("content")
		switch {
		case content.Type == gjson.String:
			inspect(content.String())
		case content.IsArray():
			content.ForEach(func(_, part gjson.Result) bool {
				if part.IsObject() {
					inspect(part.Get("text").String())
				}
				return true
			})
		}
		return true
	})
	return state
}

// ultraModeTurnKey takes the Codex turn ID: WS frames use client_metadata.turn_id /
// client_metadata.x-codex-turn-metadata, and HTTP uses the X-Codex-Turn-Metadata header.
func ultraModeTurnKey(headers http.Header, body []byte) string {
	if id := strings.TrimSpace(gjson.GetBytes(body, "client_metadata.turn_id").String()); id != "" {
		return id
	}
	raw := strings.TrimSpace(gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata").String())
	if raw == "" && headers != nil {
		raw = strings.TrimSpace(headers.Get("X-Codex-Turn-Metadata"))
	}
	if raw == "" || !gjson.Valid(raw) {
		return ""
	}
	return strings.TrimSpace(gjson.Get(raw, "turn_id").String())
}

type ultraModeTurnEntry struct {
	on   bool
	seen time.Time
}

type ultraModeTurnCache struct {
	mu      sync.Mutex
	entries map[string]ultraModeTurnEntry
}

var ultraModeTurns = &ultraModeTurnCache{entries: make(map[string]ultraModeTurnEntry)}

func (c *ultraModeTurnCache) set(key string, on bool, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= ultraModeTurnMaxEntries {
		c.pruneLocked(now)
	}
	c.entries[key] = ultraModeTurnEntry{on: on, seen: now}
}

func (c *ultraModeTurnCache) get(key string, now time.Time) (bool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return false, false
	}
	if now.Sub(entry.seen) > ultraModeTurnTTL {
		delete(c.entries, key)
		return false, false
	}
	entry.seen = now
	c.entries[key] = entry
	return entry.on, true
}

// pruneLocked clears expired entries first; if still over the limit it clears everything: losing the turn cache only affects the markers of continuation frames,
// does not affect billing or routing, and is cheaper than doing LRU on the hot path.
func (c *ultraModeTurnCache) pruneLocked(now time.Time) {
	for key, entry := range c.entries {
		if now.Sub(entry.seen) > ultraModeTurnTTL {
			delete(c.entries, key)
		}
	}
	if len(c.entries) >= ultraModeTurnMaxEntries {
		c.entries = make(map[string]ultraModeTurnEntry)
	}
}

// resolveRequestUltraMode determines whether this request is in the Codex Ultra tier.
func resolveRequestUltraMode(headers http.Header, body []byte) bool {
	now := time.Now()
	key := ultraModeTurnKey(headers, body)
	switch detectUltraModeFromBody(body) {
	case ultraModeOn:
		if key != "" {
			ultraModeTurns.set(key, true, now)
		}
		return true
	case ultraModeOff:
		if key != "" {
			ultraModeTurns.set(key, false, now)
		}
		return false
	}
	if key == "" {
		return false
	}
	on, _ := ultraModeTurns.get(key, now)
	return on
}

func cacheRequestUltraMode(c *gin.Context, on bool) {
	if c != nil {
		c.Set(requestUltraModeContextKey, on)
	}
}

func cachedRequestUltraMode(c *gin.Context) (bool, bool) {
	if c == nil {
		return false, false
	}
	value, exists := c.Get(requestUltraModeContextKey)
	if !exists {
		return false, false
	}
	on, ok := value.(bool)
	return on, ok
}

// populateUltraUsageMetaFromRequest writes the Ultra determination into the usage log. The entry point's cached
// result is used directly; if nothing is cached (such as a non-/v1/responses entry point), it falls back to determining once from the raw request body.
func populateUltraUsageMetaFromRequest(c *gin.Context, input *database.UsageLogInput) {
	if input == nil || input.Ultra {
		return
	}
	if on, ok := cachedRequestUltraMode(c); ok {
		input.Ultra = on
		return
	}
	body, ok := rawRequestBodyFromContext(c)
	if !ok {
		return
	}
	var headers http.Header
	if c != nil && c.Request != nil {
		headers = c.Request.Header
	}
	input.Ultra = resolveRequestUltraMode(headers, body)
}
