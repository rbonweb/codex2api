package proxy

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// parseRetryAfterHeader parses the Retry-After header (seconds or an HTTP date).
func parseRetryAfterHeader(value string) time.Duration {
	return parseRetryAfterHeaderAt(value, time.Now())
}

func parseRetryAfterHeaderAt(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds <= 0 || seconds > int64((1<<63-1)/int64(time.Second)) {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		if d := at.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}
