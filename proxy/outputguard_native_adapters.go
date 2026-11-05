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
// Never guess the upstream protocol blindly from the downstream client request path: Codex/Responses is always Responses SSE, and the rest dispatch by upstream protocol name.
// isNativeClaude is kept only for callers not yet migrated and no longer takes part in selection.
func selectNativeProjector(protocol string, isNativeClaude bool, isCodexResponses bool) ProtocolProjector {
	return selectNativeProjectorWithScope(protocol, isNativeClaude, isCodexResponses, "")
}

// selectNativeProjectorWithScope selects the projector for the specified attempt scope.
// Responses and Chat projectors have no per-block channel, so scope does not take part in selection.
func selectNativeProjectorWithScope(protocol string, _ bool, isCodexResponses bool, _ string) ProtocolProjector {
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
