package proxy

import (
	"encoding/json"
	"math"
	"net/http"
	"unicode/utf8"

	"github.com/codex2api/api"
	"github.com/gin-gonic/gin"
)

// Local token counting compatibility endpoint (issue #238).
//
// OpenAI-compatible clients such as LiteLLM probe the OpenAI Responses-style token counting endpoint before sending real requests:
// POST /v1/responses/input_tokens. This only does a local estimate: it does not forward upstream, does not consume account quota, and does not take part in scheduling; it only satisfies the compatibility probe.

type localTokenCountRequest struct {
	Model        string `json:"model"`
	Input        any    `json:"input,omitempty"`
	Instructions any    `json:"instructions,omitempty"`
	Tools        any    `json:"tools,omitempty"`
}

// estimateInputTokens gives a stable positive estimate based on the JSON text length of the payload
// (roughly 1 token per 3 characters, plus a small fixed overhead). Precision does not matter; the compatibility probe only needs a positive number.
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

// ResponsesInputTokens handles OpenAI /v1/responses/input_tokens compatibility requests (local estimate only).
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
