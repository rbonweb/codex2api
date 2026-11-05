package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

const (
	// codexSubagentHeader is the subagent identifier header carried by Codex internal sub-requests (such as the guardian scorer).
	codexSubagentHeader = "X-Openai-Subagent"

	// The request_kind values Codex puts in x-codex-turn-metadata (verified from the CLI 0.154 binary):
	//   turn        sampling requests for a user turn, including each resample after a tool call
	//   prewarm     warm-up request: the pre-opened connection is reused by the first turn
	//   compaction  context compaction: runs sequentially between two samples, reusing the current turn's session
	//   memory      memory consolidation: an independent root turn, started concurrently in the background, but it reuses the session's thread_id
	// The first three share a connection or a sequential action with the user turn, so they must share its lane; undeclared values are treated as turn.
	// Other values each get their own lane: it is better to replace only among the same kind than to let a background side request cut off a user turn that is in flight.
	codexRequestKindTurn       = "turn"
	codexRequestKindPrewarm    = "prewarm"
	codexRequestKindCompaction = "compaction"
)

// IsCodexWebsocketPrewarmRequest detects a warm-up request that the client explicitly declares.
func IsCodexWebsocketPrewarmRequest(headers http.Header, body []byte) bool {
	return extractCodexRequestKind(headers, body) == codexRequestKindPrewarm
}

// ResolveCodexWebsocketTransportSessionKey returns the local-only connection
// pool lane for one explicit Codex session. A Codex session tree shares
// session-id while child agents have independent thread-id values. The shared
// upstream session remains responsible for account affinity and prompt cache;
// only a genuine child/alternate thread gets a separate transport lane.
//
// The lane is derived from raw downstream identity before any account
// fingerprint convergence. Fingerprint policy controls what the upstream sees,
// not whether independent local streams must serialize on one connection.
func ResolveCodexWebsocketTransportSessionKey(upstreamSessionID string, downstreamHeaders http.Header) string {
	return ResolveCodexWebsocketTransportSessionKeyWithBody(upstreamSessionID, downstreamHeaders, nil)
}

// ResolveCodexWebsocketTransportSessionKeyWithBody is the same as ResolveCodexWebsocketTransportSessionKey
// except that it also splits lanes by request_kind / subagent identifier: background requests on the same thread that run concurrently with the user turn
// (memory consolidation, guardian scoring) no longer preempt or queue behind the user turn on the same key. When the header has no turn
// metadata (Desktop uses HTTP), it falls back to reading the same-named JSON embedded in the request body's client_metadata.
func ResolveCodexWebsocketTransportSessionKeyWithBody(upstreamSessionID string, downstreamHeaders http.Header, body []byte) string {
	upstreamSessionID = strings.TrimSpace(upstreamSessionID)
	if upstreamSessionID == "" || IsStatelessWebsocketSessionID(upstreamSessionID) {
		return upstreamSessionID
	}
	clientSessionID, clientThreadID := extractClientCodexIdentity(downstreamHeaders)
	lane := resolveCodexExecutionLane(downstreamHeaders, body, clientThreadID)
	if streamID := gjson.GetBytes(body, "stream_id").String(); streamID != "" {
		// Upstream leases are single-response today. Independent downstream
		// streams therefore need independent transport slots, even when they
		// share the same Codex thread and account affinity.
		lane += "\x00stream=" + streamID
	}
	childThread := clientThreadID != "" && !(clientSessionID != "" && clientThreadID == clientSessionID)
	if !childThread && lane == "" {
		return upstreamSessionID
	}
	seed := "codex2api:ws-transport-lane:v1\x00" + upstreamSessionID + "\x00" + clientSessionID + "\x00" + clientThreadID
	if lane != "" {
		seed += "\x00" + lane
	}
	sum := sha256.Sum256([]byte(seed))
	return "ws-thread-" + hex.EncodeToString(sum[:16])
}

// resolveCodexExecutionLane derives the execution lane suffix: the main lane (turn / prewarm / compaction / undeclared)
// returns an empty string; other request_kind values each get their own lane; when there is no thread identifier but a subagent is declared, the lane is based on the subagent value.
func resolveCodexExecutionLane(headers http.Header, body []byte, clientThreadID string) string {
	if kind := extractCodexRequestKind(headers, body); kind != "" {
		switch kind {
		case codexRequestKindTurn, codexRequestKindPrewarm, codexRequestKindCompaction:
		default:
			return "kind=" + kind
		}
	}
	if clientThreadID == "" {
		if subagent := extractCodexSubagent(headers, body); subagent != "" {
			return "subagent=" + subagent
		}
	}
	return ""
}

// extractCodexRequestKind takes request_kind from x-codex-turn-metadata: the header takes priority,
// and when the header is missing or is not valid JSON, it falls back to the same-named string embedded in the request body's client_metadata.
func extractCodexRequestKind(headers http.Header, body []byte) string {
	if headers != nil {
		if raw := strings.TrimSpace(headers.Get(codexTurnMetadataHeader)); raw != "" && gjson.Valid(raw) {
			return normalizeCodexRequestKind(gjson.Get(raw, "request_kind"))
		}
	}
	if len(body) == 0 {
		return ""
	}
	embedded := gjson.GetBytes(body, "client_metadata."+strings.ToLower(codexTurnMetadataHeader))
	if embedded.Type != gjson.String || !gjson.Valid(embedded.String()) {
		return ""
	}
	return normalizeCodexRequestKind(gjson.Get(embedded.String(), "request_kind"))
}

func normalizeCodexRequestKind(value gjson.Result) string {
	if value.Type != gjson.String {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(value.String()))
}

func extractCodexSubagent(headers http.Header, body []byte) string {
	if headers != nil {
		if value := strings.TrimSpace(headers.Get(codexSubagentHeader)); value != "" {
			return strings.ToLower(value)
		}
	}
	if len(body) == 0 {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "client_metadata."+strings.ToLower(codexSubagentHeader)).String()))
}
