package proxy

import (
	"errors"
	"strings"

	"github.com/codex2api/security/outputguard"
)

// errModelOutputGuardTriggered is the per-request sentinel returned by the native HTTP streaming forwarder after the Guard trips:
// it is never sent downstream or logged; it only lets the caller recognize that "the stream has been safely terminated".
var errModelOutputGuardTriggered = errors.New("model output guard triggered")

// nativeGuardCoreConfig builds the per-request Guard config from the currently applied runtime snapshot.
// The snapshot is taken once at request start, so new generations from the background poller do not alter in-flight requests.
func nativeGuardCoreConfig() outputguard.Config {
	applied := CurrentOutputGuardConfig()
	return outputguard.Config{
		Enabled:               applied.Enabled,
		MaxToolArgumentBytes:  applied.MaxToolArgumentBytes,
		MaxToolArgumentDeltas: applied.MaxToolArgumentDeltas,
		MaxChannelBytes:       applied.MaxChannelBytes,
		MaxRequestBytes:       applied.MaxRequestBytes,
	}
}

// selectNativeProjector selects the real upstream projector for the upstream channel.
// Never guess the upstream protocol blindly from the client's downstream request path: Codex/Responses is always Responses SSE, and everything else is dispatched by the upstream protocol name.
func selectNativeProjector(protocol string, isCodexResponses bool) ProtocolProjector {
	if isCodexResponses {
		return NewResponsesProjector()
	}
	norm := strings.ToLower(strings.TrimSpace(protocol))
	switch norm {
	case "chat", "chat_completions", "chatcompletions":
		return NewChatProjector()
	case "responses":
		return NewResponsesProjector()
	default:
		return NewChatProjector()
	}
}
