package proxy

import (
	"bytes"
	"context"
	"strings"
	"unicode/utf8"

	"github.com/tidwall/gjson"
)

// Observed state values measure around 300 characters, so a margin of one order of magnitude is enough; anything over the limit is always dropped,
// because a truncated state can neither be reused nor be a reliable troubleshooting aid.
const maxObservedCodexTurnStateBytes = 4096

// observedCodexTurnState normalizes an observed value: it only accepts a single-line visible string and drops anything over the limit.
func observedCodexTurnState(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxObservedCodexTurnStateBytes || !utf8.ValidString(value) {
		return ""
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return ""
		}
	}
	return value
}

var codexTurnStateFrameNeedles = [][]byte{[]byte("turn-state"), []byte("Turn-State")}

// codexTurnStateFromFrame looks for the upstream-echoed turn state in a WS event frame. In the official contract, the WS path
// takes its value from the handshake response header or the response.metadata event; here it matches the key name exactly (case-insensitive) across several
// known carrier locations, and returns empty if not found. It first runs a zero-allocation substring pre-check so that every frame does not have to parse JSON.
func codexTurnStateFromFrame(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	mentioned := false
	for _, needle := range codexTurnStateFrameNeedles {
		if bytes.Contains(payload, needle) {
			mentioned = true
			break
		}
	}
	if !mentioned {
		return ""
	}
	root := gjson.ParseBytes(payload)
	if !root.IsObject() {
		return ""
	}
	for _, path := range []string{"headers", "response.headers", "response.client_metadata", "client_metadata", "response.metadata", "metadata", "response", ""} {
		object := root
		if path != "" {
			object = root.Get(path)
		}
		if !object.IsObject() {
			continue
		}
		state := ""
		object.ForEach(func(key, value gjson.Result) bool {
			if strings.EqualFold(key.String(), codexTurnStateHeader) && value.Type == gjson.String {
				state = value.String()
				return false
			}
			return true
		})
		if state = observedCodexTurnState(state); state != "" {
			return state
		}
	}
	return ""
}

// ObserveCodexTurnStateFrame is called by the WS relay while forwarding frame by frame: when it finds the upstream-echoed turn state,
// it records it in the trace of this attempt (the usage log uses this to show "Echoed Turn State").
func ObserveCodexTurnStateFrame(ctx context.Context, payload []byte) string {
	state := codexTurnStateFromFrame(payload)
	if state != "" {
		noteUpstreamTurnState(ctx, state)
	}
	return state
}
