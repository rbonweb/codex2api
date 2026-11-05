package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/api"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

const (
	// GuardWriteDeadlineTimeout is the bounded timeout for writing the guard terminal frame after the response headers have been committed.
	// It only bounds best-effort downstream notification and never participates in or blocks upstream cancellation.
	GuardWriteDeadlineTimeout = 5 * time.Second
)

// ApplyDownstreamWriteDeadline applies a bounded timeout to downstream writes through http.ResponseController.
// timeout<=0 clears the deadline. If the underlying layer does not support it, it returns
// http.ErrNotSupported, and the caller must give up the write rather than fall back to a synchronous write that may block without bound.
func ApplyDownstreamWriteDeadline(w http.ResponseWriter, timeout time.Duration) error {
	if w == nil {
		return errors.New("nil response writer")
	}
	rc := http.NewResponseController(w)
	if rc == nil {
		return http.ErrNotSupported
	}
	if timeout <= 0 {
		return rc.SetWriteDeadline(time.Time{})
	}
	return rc.SetWriteDeadline(time.Now().Add(timeout))
}

// FormatGuardTerminalEnvelope builds the redacted guard terminal frame for each protocol.
// The returned status is used only when the response headers have not been committed (502); it is ignored once they have.
// No protocol carries the internal trigger reason.
func FormatGuardTerminalEnvelope(protocol string, streamID string) ([]byte, int, error) {
	normProto := strings.ToLower(strings.TrimSpace(protocol))
	switch normProto {
	case "responses":
		payload, err := json.Marshal(gin.H{
			"type": "response.failed",
			"response": gin.H{
				"created_at": time.Now().Unix(),
				"status":     "failed",
				"error": gin.H{
					"message": GuardTriggeredPublicMessage,
					"type":    api.ErrorTypeUpstream,
					"code":    GuardTriggeredPublicCode,
				},
			},
		})
		if err != nil {
			return nil, 0, err
		}
		return []byte("data: " + string(payload) + "\n\n"), http.StatusOK, nil

	case "chat":
		payload, err := json.Marshal(gin.H{
			"error": gin.H{
				"message": GuardTriggeredPublicMessage,
				"type":    api.ErrorTypeUpstream,
				"code":    GuardTriggeredPublicCode,
			},
		})
		if err != nil {
			return nil, 0, err
		}
		return []byte("data: " + string(payload) + "\n\n"), http.StatusOK, nil

	case "anthropic":
		payload, err := json.Marshal(gin.H{
			"type":  "error",
			"error": gin.H{"type": "api_error", "message": GuardTriggeredPublicMessage},
		})
		if err != nil {
			return nil, 0, err
		}
		return []byte("event: error\ndata: " + string(payload) + "\n\n"), http.StatusOK, nil

	case "gemini":
		// The native Gemini SDK does not parse SSE; the error must be bare JSON without the data: prefix.
		payload, err := json.Marshal(gin.H{
			"error": gin.H{"code": http.StatusBadGateway, "message": GuardTriggeredPublicMessage, "status": "UNAVAILABLE"},
		})
		if err != nil {
			return nil, 0, err
		}
		return payload, http.StatusOK, nil

	case "websocket":
		payload, err := json.Marshal(struct {
			Type     string        `json:"type"`
			StreamID string        `json:"stream_id,omitempty"`
			Status   int           `json:"status"`
			Error    *api.APIError `json:"error"`
		}{
			Type:     "error",
			StreamID: streamID,
			Status:   http.StatusBadGateway,
			Error:    api.NewAPIError(GuardTriggeredPublicCode, GuardTriggeredPublicMessage, api.ErrorTypeUpstream),
		})
		if err != nil {
			return nil, 0, err
		}
		return payload, http.StatusOK, nil

	default:
		payload, err := json.Marshal(gin.H{
			"error": gin.H{"message": GuardTriggeredPublicMessage, "type": api.ErrorTypeUpstream, "code": GuardTriggeredPublicCode},
		})
		if err != nil {
			return nil, 0, err
		}
		return payload, http.StatusBadGateway, nil
	}
}

// SendDownstreamGuardTerminalHTTP makes a best-effort attempt to send the guard terminal state to the downstream HTTP client.
//
// If the response headers are not committed, it returns a 502 JSON error directly.
// If the streaming response headers are committed, it takes the downstream write lock non-blockingly via TryLock, and inside the lock uses ResponseController
// to apply a 5s write timeout and then write the protocol terminal frame. If the lock is held, the deadline is unsupported, the connection is gone,
// or the write fails, it gives up immediately (clientGone=true). It never starts an orphan goroutine to keep writing.
//
// Regardless of success, this function never blocks or delays upstream cancellation: the caller must first execute
// GuardTerminationCoordinator.HandleTripped.
func SendDownstreamGuardTerminalHTTP(c *gin.Context, mu *sync.Mutex, clientGone *bool, protocol string, reason string) error {
	if c == nil || c.Writer == nil {
		if clientGone != nil {
			*clientGone = true
		}
		return errors.New("nil context or writer")
	}

	if !c.Writer.Written() {
		c.Header("Content-Type", "application/json; charset=utf-8")
		c.JSON(http.StatusBadGateway, gin.H{
			"error": gin.H{"message": GuardTriggeredPublicMessage, "type": api.ErrorTypeUpstream, "code": GuardTriggeredPublicCode},
		})
		if clientGone != nil {
			*clientGone = true
		}
		return nil
	}

	if mu != nil {
		if !mu.TryLock() {
			if clientGone != nil {
				*clientGone = true
			}
			return errors.New("downstream write lock busy; dropped guard terminal notification")
		}
		defer mu.Unlock()
	}

	if c.Request != nil {
		if err := c.Request.Context().Err(); err != nil {
			if clientGone != nil {
				*clientGone = true
			}
			return err
		}
	}

	if err := ApplyDownstreamWriteDeadline(c.Writer, GuardWriteDeadlineTimeout); err != nil {
		if clientGone != nil {
			*clientGone = true
		}
		return fmt.Errorf("set write deadline failed (%w); safely dropped write", err)
	}
	defer func() { _ = ApplyDownstreamWriteDeadline(c.Writer, 0) }()

	envelope, _, err := FormatGuardTerminalEnvelope(protocol, "")
	if err != nil {
		return err
	}
	if _, err := c.Writer.Write(envelope); err != nil {
		if clientGone != nil {
			*clientGone = true
		}
		return err
	}
	if flusher, ok := c.Writer.(http.Flusher); ok {
		flusher.Flush()
	}
	if clientGone != nil {
		*clientGone = true
	}
	return nil
}

// SendDownstreamGuardTerminalWS writes an error frame carrying stream_id to the Responses WebSocket connection,
// redacted. The write timeout is bounded; the caller is responsible for closing the connection afterwards.
//
// Concurrent write contract: gorilla/websocket forbids concurrent writes on the same conn (concurrent writes panic).
// The caller must guarantee there are no concurrent writes on the same conn (including the streaming forwarding goroutine). This function does no
// connection-level write serialization and does not implicitly wait for the forwarding side's writes to finish. When wsrelay is wired up later, the caller must
// use the same write lock or a single writer goroutine to guarantee mutual exclusion.
func SendDownstreamGuardTerminalWS(conn *websocket.Conn, streamID string, reason string) error {
	if conn == nil {
		return errors.New("nil websocket conn")
	}
	envelope, _, err := FormatGuardTerminalEnvelope("websocket", streamID)
	if err != nil {
		return err
	}
	_ = conn.SetWriteDeadline(time.Now().Add(GuardWriteDeadlineTimeout))
	defer func() { _ = conn.SetWriteDeadline(time.Time{}) }()
	return conn.WriteMessage(websocket.TextMessage, envelope)
}
