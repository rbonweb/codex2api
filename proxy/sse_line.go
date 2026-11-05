package proxy

import (
	"bufio"
	"errors"
	"io"
	"math"
)

// SSE wire limits are resource bounds, not a declaration that larger events
// are invalid upstream. Base64 media and large tool results can exceed them.
const maxSSELineBytes = 8 << 20

const maxSSEEventBytes = 8 << 20

var errSSELineTooLong = errors.New("upstream SSE line exceeds limit")

var errSSEEventTooLarge = errors.New("upstream SSE event exceeds limit")

// readBoundedSSELine reads one line, with memory use not exceeding limit (excluding bufio's own buffer).
func readBoundedSSELine(reader *bufio.Reader, limit int) ([]byte, error) {
	var line []byte
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(line)+len(fragment) > limit {
			return nil, errSSELineTooLong
		}
		line = append(line, fragment...)
		if err != bufio.ErrBufferFull {
			return line, err
		}
	}
}

// -------------------------------------------------------------------------
// Additions: exported constant aliases and a bounded reader for non-streaming reads (strictly keeping compatibility with the existing lowercase symbols)
// -------------------------------------------------------------------------

const (
	MaxSSELineBytes      = maxSSELineBytes
	MaxSSEEventBytes     = maxSSEEventBytes
	MaxResponseBodyBytes = 16 << 20 // 16 MiB (default limit for non-streaming response bodies)
)

var (
	ErrSSELineTooLong       = errSSELineTooLong
	ErrSSEEventTooLarge     = errSSEEventTooLarge
	ErrResponseBodyTooLarge = errors.New("response_body_too_large")
)

// ReadBoundedResponseBody reads a non-streaming response body with a bound.
func ReadBoundedResponseBody(body io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		limit = MaxResponseBodyBytes
	}
	// Reserving +1 to detect overflow needs limit+1; at MaxInt64, adding one overflows to a negative value, which causes
	// io.LimitReader to return EOF immediately and silently return empty data, so clamp first.
	if limit > math.MaxInt64-1 {
		limit = math.MaxInt64 - 1
	}
	lr := io.LimitReader(body, limit+1)
	data, err := io.ReadAll(lr)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, ErrResponseBodyTooLarge
	}
	return data, nil
}
