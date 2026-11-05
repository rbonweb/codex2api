package proxy

import (
	"errors"
	"strings"

	"github.com/codex2api/security/outputguard"
)

// errModelOutputGuardTriggered 是 Native HTTP 流式转发在 Guard 命中后返回的
// 请求内哨兵：它不会进入下游或日志，只用于让调用方识别“流已被安全终止”。
var errModelOutputGuardTriggered = errors.New("model output guard triggered")

// nativeGuardCoreConfig 从当前已应用的运行时快照构造请求级 Guard 配置。
// 快照在请求开始时取一次，后台轮询器的新 generation 不会篡改在途请求。
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

// selectNativeProjector 为上游通道选择真实 upstream projector。
// 严禁按客户端下游请求路径盲目推测上游协议：Codex/Responses 永远是 Responses SSE，其余按上游协议名分派。
// isNativeClaude 仅为兼容尚未迁移的调用方而保留，已不再参与选择。
func selectNativeProjector(protocol string, isNativeClaude bool, isCodexResponses bool) ProtocolProjector {
	return selectNativeProjectorWithScope(protocol, isNativeClaude, isCodexResponses, "")
}

// selectNativeProjectorWithScope 为指定 attempt 作用域选择投影器。
// Responses 与 Chat 投影器没有按 block 区分的通道，因此 scope 不参与选择。
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
