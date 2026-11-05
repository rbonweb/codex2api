package wsrelay

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"
)

const handshakeErrorBodyLimit = 4 << 10 // 4 KiB

// HandshakeHTTPError is the structured error returned when an upstream HTTP error response is received during the handshake:
// besides the human-readable error text, it keeps the original status code, response headers, and the normalized JSON body,
// so upper layers can restore authentication or rate-limit failures to their real status codes, instead of a generic transport error.
type HandshakeHTTPError struct {
	StatusCode int
	Header     http.Header
	// Body is the normalized text of the upstream error body (JSON compacted, non-JSON whitespace collapsed),
	// so error.code / error.message can be parsed directly with gjson.
	Body string
	msg  string
}

func (e *HandshakeHTTPError) Error() string { return e.msg }

// UpstreamStatusCode and UpstreamErrorBody let the proxy's continuous retry
// selector inspect a handshake failure without importing this package (which
// would create an import cycle). The regular finite retry path still treats
// the error as a transport failure unless its legacy conversion list applies.
func (e *HandshakeHTTPError) UpstreamStatusCode() int {
	if e == nil {
		return 0
	}
	return e.StatusCode
}

func (e *HandshakeHTTPError) UpstreamErrorBody() []byte {
	if e == nil || strings.TrimSpace(e.Body) == "" {
		return nil
	}
	return []byte(e.Body)
}

// handshakeAccountErrorHTTPResponse converts handshake-phase errors that need their own classification into an HTTP response carrying the real
// status code and the upstream's original error body, so callers can reuse the existing non-2xx branches: the usage log
// records the real status code, applyCooldownForModel's unauthorized cooldown / missing_scope special case /
// deactivated_workspace marking, and the separate rate-limit retry budget for 429. Covers:
//   - 401: token expired or revoked;
//   - 402: rejected on account billing grounds, such as a deactivated workspace (deactivated_workspace); if it stayed
//     at the transport error layer, accounts with a banned team space would stay in the pool as "available" and be dialed over and over.
//   - 429: must consume the rate-limit budget and keep Retry-After; it must not wrongly spend the ordinary transport budget.
//
// 403 is not converted: a handshake 403 may come from a Cloudflare block (egress IP dimension), and classifying it as an account error
// would cause false hits; it, together with 5xx/503, keeps the transport error semantics.
func handshakeAccountErrorHTTPResponse(err error) (*http.Response, bool) {
	var hs *HandshakeHTTPError
	if !errors.As(err, &hs) {
		return nil, false
	}
	if hs.StatusCode != http.StatusUnauthorized &&
		hs.StatusCode != http.StatusPaymentRequired &&
		hs.StatusCode != http.StatusTooManyRequests {
		return nil, false
	}
	header := http.Header{}
	if hs.Header != nil {
		header = hs.Header.Clone()
	}
	return &http.Response{
		StatusCode: hs.StatusCode,
		Status:     fmt.Sprintf("%d %s", hs.StatusCode, http.StatusText(hs.StatusCode)),
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(hs.Body)),
	}, true
}

// formatDialHandshakeError wraps a Dial failure into a diagnosable error.
// gorilla/websocket still returns *http.Response on a bad handshake, and has already pre-read up to 1KB of the body;
// here we add the HTTP status code, key response headers, and the upstream body (JSON is attached as-is, so the frontend can pretty-print it).
func formatDialHandshakeError(err error, resp *http.Response) error {
	if err == nil {
		return nil
	}
	if resp == nil {
		return fmt.Errorf("websocket handshake failed: %w", err)
	}

	status := resp.StatusCode
	statusText := strings.TrimSpace(http.StatusText(status))
	body := readHTTPErrorBody(resp)
	headers := handshakeErrorHeaderSnippet(resp.Header)

	var b strings.Builder
	b.WriteString("websocket handshake failed: ")
	b.WriteString(err.Error())
	if status > 0 {
		b.WriteString(fmt.Sprintf(" (HTTP %d", status))
		if statusText != "" {
			b.WriteString(" ")
			b.WriteString(statusText)
		}
		b.WriteString(")")
	}
	if headers != "" {
		b.WriteString("; ")
		b.WriteString(headers)
	}
	if body != "" {
		// The prefix ends with a colon, so the frontend formatTestErrorMessage splits the following JSON out and pretty-prints it.
		b.WriteString(":\n")
		b.WriteString(body)
	}
	return &HandshakeHTTPError{
		StatusCode: status,
		Header:     resp.Header.Clone(),
		Body:       body,
		msg:        b.String(),
	}
}

// formatFailedHandshakeHTTPBody builds the body text when the handshake HTTP response status is not 2xx/101.
func formatFailedHandshakeHTTPBody(statusCode int, resp *http.Response) string {
	body := readHTTPErrorBody(resp)
	headers := ""
	if resp != nil {
		headers = handshakeErrorHeaderSnippet(resp.Header)
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("websocket handshake failed: HTTP %d", statusCode))
	if text := strings.TrimSpace(http.StatusText(statusCode)); text != "" {
		b.WriteString(" ")
		b.WriteString(text)
	}
	if headers != "" {
		b.WriteString("; ")
		b.WriteString(headers)
	}
	if body != "" {
		b.WriteString(":\n")
		b.WriteString(body)
	}
	return b.String()
}

func handshakeErrorHeaderSnippet(h http.Header) string {
	if h == nil {
		return ""
	}
	var parts []string
	for _, key := range []string{
		"Cf-Ray",
		"X-Request-Id",
		"X-Openai-Request-Id",
		"Openai-Organization",
		"Www-Authenticate",
		"X-Error-Code",
		"X-Error-Message",
	} {
		if v := strings.TrimSpace(h.Get(key)); v != "" {
			parts = append(parts, key+"="+truncateRunes(v, 120))
		}
	}
	return strings.Join(parts, ", ")
}

// readHTTPErrorBody reads the upstream error body; JSON is normalized to compact valid JSON (the frontend pretty-prints it again),
// and non-JSON is whitespace-collapsed. It refills resp.Body for later reads.
func readHTTPErrorBody(resp *http.Response) string {
	if resp == nil || resp.Body == nil {
		return ""
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, handshakeErrorBodyLimit))
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(raw))

	text := strings.TrimSpace(string(raw))
	if text == "" {
		return ""
	}

	// Keep the upstream JSON structure as far as possible: decode and then compact-encode, removing extra whitespace without dropping fields.
	if json.Valid([]byte(text)) {
		var v any
		if err := json.Unmarshal([]byte(text), &v); err == nil {
			if compact, err := json.Marshal(v); err == nil {
				return truncateRunes(string(compact), 2000)
			}
		}
		return truncateRunes(text, 2000)
	}

	// Roughly truncate plain text / HTML.
	text = strings.Join(strings.Fields(text), " ")
	return truncateRunes(text, 500)
}

func truncateRunes(s string, max int) string {
	if max <= 0 || s == "" {
		return s
	}
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	var b strings.Builder
	b.Grow(max*3 + 1)
	n := 0
	for _, r := range s {
		if n >= max {
			break
		}
		b.WriteRune(r)
		n++
	}
	b.WriteString("…")
	return b.String()
}
