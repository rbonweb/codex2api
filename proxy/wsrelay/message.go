package wsrelay

import (
	"time"
)

// ==================== WebSocket message frame format ====================

// MessageType WebSocket message type constants
type MessageType string

const (
	// HTTP-related message types
	MessageTypeHTTPRequest  MessageType = "http_request"  // HTTP request
	MessageTypeHTTPResponse MessageType = "http_response" // HTTP response (non-streaming)

	// Streaming message types
	MessageTypeStreamStart MessageType = "stream_start" // stream start
	MessageTypeStreamChunk MessageType = "stream_chunk" // stream chunk
	MessageTypeStreamEnd   MessageType = "stream_end"   // stream end

	// Error message type
	MessageTypeError MessageType = "error" // error

	// Control message types
	MessageTypePing MessageType = "ping" // heartbeat ping
	MessageTypePong MessageType = "pong" // heartbeat pong
)

// Message is the WebSocket message frame structure.
type Message struct {
	// Message type
	Type MessageType `json:"type"`

	// Request ID (used to match requests and responses)
	RequestID string `json:"request_id,omitempty"`

	// Session ID (used for prompt cache binding)
	SessionID string `json:"session_id,omitempty"`

	// HTTP status code (only used for http_response and error)
	StatusCode int `json:"status_code,omitempty"`

	// Message content (the request or response body in JSON format)
	Content []byte `json:"content,omitempty"`

	// Error message (only used for the error type)
	Error string `json:"error,omitempty"`

	// Timestamp (Unix milliseconds)
	Timestamp int64 `json:"timestamp,omitempty"`
}

// NewHTTPRequestMessage creates an HTTP request message.
func NewHTTPRequestMessage(requestID, sessionID string, content []byte) *Message {
	return &Message{
		Type:      MessageTypeHTTPRequest,
		RequestID: requestID,
		SessionID: sessionID,
		Content:   content,
		Timestamp: time.Now().UnixMilli(),
	}
}

// NewHTTPResponseMessage creates an HTTP response message (non-streaming).
func NewHTTPResponseMessage(requestID string, statusCode int, content []byte) *Message {
	return &Message{
		Type:       MessageTypeHTTPResponse,
		RequestID:  requestID,
		StatusCode: statusCode,
		Content:    content,
		Timestamp:  time.Now().UnixMilli(),
	}
}

// NewStreamStartMessage creates a stream start message.
func NewStreamStartMessage(requestID string, statusCode int) *Message {
	return &Message{
		Type:       MessageTypeStreamStart,
		RequestID:  requestID,
		StatusCode: statusCode,
		Timestamp:  time.Now().UnixMilli(),
	}
}

// NewStreamChunkMessage creates a stream chunk message.
func NewStreamChunkMessage(requestID string, content []byte) *Message {
	return &Message{
		Type:      MessageTypeStreamChunk,
		RequestID: requestID,
		Content:   content,
		Timestamp: time.Now().UnixMilli(),
	}
}

// NewStreamEndMessage creates a stream end message.
func NewStreamEndMessage(requestID string) *Message {
	return &Message{
		Type:      MessageTypeStreamEnd,
		RequestID: requestID,
		Timestamp: time.Now().UnixMilli(),
	}
}

// NewErrorMessage creates an error message.
func NewErrorMessage(requestID string, statusCode int, errMsg string) *Message {
	return &Message{
		Type:       MessageTypeError,
		RequestID:  requestID,
		StatusCode: statusCode,
		Error:      errMsg,
		Timestamp:  time.Now().UnixMilli(),
	}
}

// NewPingMessage creates a ping heartbeat message.
func NewPingMessage() *Message {
	return &Message{
		Type:      MessageTypePing,
		Timestamp: time.Now().UnixMilli(),
	}
}

// NewPongMessage creates a pong heartbeat response message.
func NewPongMessage() *Message {
	return &Message{
		Type:      MessageTypePong,
		Timestamp: time.Now().UnixMilli(),
	}
}
