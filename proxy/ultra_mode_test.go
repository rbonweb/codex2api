package proxy

import (
	"net/http"
	"testing"
	"time"
)

const (
	ultraTestActiveMsg = `{"type":"message","role":"developer","content":[{"type":"input_text","text":"<multi_agent_mode>Proactive multi-agent delegation is active. Any earlier developer instruction requiring an explicit user request before spawning sub-agents no longer applies.</multi_agent_mode>"}]}`
	ultraTestOffMsg    = `{"type":"message","role":"developer","content":[{"type":"input_text","text":"<multi_agent_mode>Any earlier instruction enabling proactive multi-agent delegation no longer applies. Do not spawn sub-agents unless the user explicitly asks.</multi_agent_mode>"}]}`
	ultraTestUserMsg   = `{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`
)

func TestDetectUltraModeFromBody(t *testing.T) {
	cases := []struct {
		name string
		body string
		want ultraModeState
	}{
		{"no marker", `{"model":"gpt-6-astra","input":[` + ultraTestUserMsg + `]}`, ultraModeUnknown},
		{"active", `{"input":[` + ultraTestUserMsg + `,` + ultraTestActiveMsg + `,` + ultraTestUserMsg + `]}`, ultraModeOn},
		{"off only", `{"input":[` + ultraTestOffMsg + `,` + ultraTestUserMsg + `]}`, ultraModeOff},
		// Within history, on then off: the last one wins (measured: a Max turn carries three entries OFF→ACTIVE→OFF).
		{"last wins off", `{"input":[` + ultraTestOffMsg + `,` + ultraTestActiveMsg + `,` + ultraTestOffMsg + `]}`, ultraModeOff},
		{"last wins on", `{"input":[` + ultraTestOffMsg + `,` + ultraTestActiveMsg + `]}`, ultraModeOn},
		// The same text in a user message does not count: only the developer role is recognized.
		{"user quoting marker", `{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"<multi_agent_mode>Proactive multi-agent delegation is active</multi_agent_mode>"}]}]}`, ultraModeUnknown},
		{"string content", `{"input":[{"role":"developer","content":"<multi_agent_mode>Proactive multi-agent delegation is active</multi_agent_mode>"}]}`, ultraModeOn},
		{"input not array", `{"input":"<multi_agent_mode>Proactive multi-agent delegation is active</multi_agent_mode>"}`, ultraModeUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := detectUltraModeFromBody([]byte(tc.body)); got != tc.want {
				t.Fatalf("detectUltraModeFromBody = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestResolveRequestUltraModeInheritsWithinTurn(t *testing.T) {
	ultraModeTurns.mu.Lock()
	ultraModeTurns.entries = make(map[string]ultraModeTurnEntry)
	ultraModeTurns.mu.Unlock()

	first := `{"input":[` + ultraTestActiveMsg + `,` + ultraTestUserMsg + `],"client_metadata":{"turn_id":"turn-ultra"}}`
	if !resolveRequestUltraMode(nil, []byte(first)) {
		t.Fatal("first frame with active marker should be ultra")
	}
	// Continuation frame: only tool output and no history, so it inherits via turn_id.
	continuation := `{"input":[{"type":"custom_tool_call_output","call_id":"c1","output":"ok"}],"previous_response_id":"resp_1","client_metadata":{"turn_id":"turn-ultra"}}`
	if !resolveRequestUltraMode(nil, []byte(continuation)) {
		t.Fatal("continuation frame in the same turn should inherit ultra")
	}
	// A continuation frame from another turn must not leak across.
	other := `{"input":[{"type":"custom_tool_call_output","call_id":"c2","output":"ok"}],"previous_response_id":"resp_2","client_metadata":{"turn_id":"turn-other"}}`
	if resolveRequestUltraMode(nil, []byte(other)) {
		t.Fatal("unknown turn without marker must not be ultra")
	}
	// Explicitly turned off later in the same turn → overwrites the cache.
	off := `{"input":[` + ultraTestActiveMsg + `,` + ultraTestOffMsg + `],"client_metadata":{"turn_id":"turn-ultra"}}`
	if resolveRequestUltraMode(nil, []byte(off)) {
		t.Fatal("explicit off marker should win")
	}
	if resolveRequestUltraMode(nil, []byte(continuation)) {
		t.Fatal("continuation after explicit off should not be ultra")
	}
	// A continuation frame without turn_id does no inheritance at all.
	noTurn := `{"input":[{"type":"custom_tool_call_output","call_id":"c3","output":"ok"}],"previous_response_id":"resp_3"}`
	if resolveRequestUltraMode(nil, []byte(noTurn)) {
		t.Fatal("frame without turn id must not inherit")
	}
}

func TestUltraModeTurnKeySources(t *testing.T) {
	if got := ultraModeTurnKey(nil, []byte(`{"client_metadata":{"turn_id":"t1"}}`)); got != "t1" {
		t.Fatalf("client_metadata.turn_id: got %q", got)
	}
	embedded := `{"client_metadata":{"x-codex-turn-metadata":"{\"turn_id\":\"t2\",\"request_kind\":\"turn\"}"}}`
	if got := ultraModeTurnKey(nil, []byte(embedded)); got != "t2" {
		t.Fatalf("embedded turn metadata: got %q", got)
	}
	headers := http.Header{}
	headers.Set("X-Codex-Turn-Metadata", `{"turn_id":"t3"}`)
	if got := ultraModeTurnKey(headers, []byte(`{"input":[]}`)); got != "t3" {
		t.Fatalf("header turn metadata: got %q", got)
	}
	if got := ultraModeTurnKey(nil, []byte(`{"input":[]}`)); got != "" {
		t.Fatalf("no turn id: got %q", got)
	}
}

func TestUltraModeTurnCacheExpiry(t *testing.T) {
	cache := &ultraModeTurnCache{entries: make(map[string]ultraModeTurnEntry)}
	now := time.Now()
	cache.set("t", true, now)
	if on, ok := cache.get("t", now.Add(time.Minute)); !ok || !on {
		t.Fatal("fresh entry should be readable")
	}
	if _, ok := cache.get("t", now.Add(ultraModeTurnTTL+2*time.Minute)); ok {
		t.Fatal("expired entry should be dropped")
	}
}
