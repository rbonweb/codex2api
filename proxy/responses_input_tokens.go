package proxy

import (
	"encoding/json"
	"math"
	"net/http"
	"unicode/utf8"

	"github.com/codex2api/api"
	"github.com/gin-gonic/gin"
)

// 本地 token 计数兼容端点（issue #238）。
//
// LiteLLM 等 OpenAI 兼容客户端在发真实请求前，会探测 OpenAI Responses 风格的 token 计数端点：
// POST /v1/responses/input_tokens。这里只做本地估算：不转发上游、不消耗账号额度、不参与调度，仅满足兼容性探测。

type localTokenCountRequest struct {
	Model        string `json:"model"`
	Input        any    `json:"input,omitempty"`
	Instructions any    `json:"instructions,omitempty"`
	Tools        any    `json:"tools,omitempty"`
}

// estimateInputTokens 基于 payload 的 JSON 文本长度给出一个稳定的正数估算
// （约每 3 个字符 1 token，再加少量固定开销）。精度不重要，兼容探测只需正数。
func estimateInputTokens(value any) int {
	data, err := json.Marshal(value)
	if err != nil {
		return 1
	}
	count := utf8.RuneCountInString(string(data))
	if count == 0 {
		return 0
	}
	return int(math.Ceil(float64(count)/3.0)) + 8
}

// ResponsesInputTokens 处理 OpenAI /v1/responses/input_tokens 兼容请求（仅本地估算）。
func (h *Handler) ResponsesInputTokens(c *gin.Context) {
	var req localTokenCountRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidRequest, "Invalid JSON request body", api.ErrorTypeInvalidRequest))
		return
	}

	payload := map[string]any{
		"instructions": req.Instructions,
		"input":        req.Input,
		"tools":        req.Tools,
	}

	c.JSON(http.StatusOK, gin.H{"input_tokens": estimateInputTokens(payload)})
}
