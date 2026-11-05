package proxy

import (
	"net/http"

	"github.com/codex2api/api"
	"github.com/gin-gonic/gin"
)

// AudioTranscriptions answers POST /v1/audio/transcriptions until the transcription slice replaces this stub.
func (h *Handler) AudioTranscriptions(c *gin.Context) {
	api.SendErrorWithStatus(c, api.NewAPIError(
		api.ErrorCode("not_implemented"),
		"/v1/audio/transcriptions is not available yet",
		api.ErrorTypeServer,
	), http.StatusNotImplemented)
}
