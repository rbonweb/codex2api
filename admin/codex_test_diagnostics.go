package admin

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/codex2api/auth"
	"github.com/codex2api/security/promptfilter"
	"github.com/tidwall/gjson"
)

// Codex connection-test diagnostics: record the HTTP status of this Responses probe, the per-segment timings, and what the upstream returned:
// request/response IDs, the x-codex-* usage window headers, the final usage, and the redacted response headers and raw body.
// It only describes this request; fields that were not observed stay unset and are not filled with zero values.

const codexTestBodyLimit = 64 << 10

type codexTestHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// codexTestWindow is a verbatim projection of the x-codex-primary-* / x-codex-secondary-* triplet,
// window_minutes decides whether it is a 5h (300) or 7d (10080) window; the frontend labels it by minutes.
type codexTestWindow struct {
	UsedPercent       *float64 `json:"used_percent,omitempty"`
	WindowMinutes     *float64 `json:"window_minutes,omitempty"`
	ResetAfterSeconds *float64 `json:"reset_after_seconds,omitempty"`
}

func (w *codexTestWindow) empty() bool {
	return w == nil || (w.UsedPercent == nil && w.WindowMinutes == nil && w.ResetAfterSeconds == nil)
}

type codexTestUsage struct {
	InputTokens     *int64 `json:"input_tokens,omitempty"`
	OutputTokens    *int64 `json:"output_tokens,omitempty"`
	TotalTokens     *int64 `json:"total_tokens,omitempty"`
	CachedTokens    *int64 `json:"cached_input_tokens,omitempty"`
	ReasoningTokens *int64 `json:"reasoning_output_tokens,omitempty"`
}

type codexTestDiagnostics struct {
	HTTPStatus     int    `json:"http_status,omitempty"`
	DurationMS     *int64 `json:"duration_ms,omitempty"`
	HeadersMS      *int64 `json:"headers_ms,omitempty"`
	FirstFrameMS   *int64 `json:"first_frame_ms,omitempty"`
	FirstContentMS *int64 `json:"first_content_ms,omitempty"`
	Model          string `json:"model"`
	ResponseModel  string `json:"response_model,omitempty"`
	Transport      string `json:"transport,omitempty"`
	RequestID      string `json:"request_id,omitempty"`
	ResponseID     string `json:"response_id,omitempty"`
	CFRay          string `json:"cf_ray,omitempty"`
	PlanType       string `json:"plan_type,omitempty"`
	// Safety buffering: the upstream may hold back output for an extra review; enabled only means this model has the capability turned on,
	// faster_model is the switch target of the official CLI's "Retry with a faster model"; buffered
	// means this round was actually buffered (event-level safety_buffering=true).
	SafetyBufferingEnabled     *bool             `json:"safety_buffering_enabled,omitempty"`
	SafetyBufferingFasterModel string            `json:"safety_buffering_faster_model,omitempty"`
	SafetyBuffered             bool              `json:"safety_buffered,omitempty"`
	ResponseStatus             string            `json:"response_status,omitempty"`
	IncompleteReason           string            `json:"incomplete_reason,omitempty"`
	ErrorType                  string            `json:"error_type,omitempty"`
	ErrorCode                  string            `json:"error_code,omitempty"`
	PrimaryWindow              *codexTestWindow  `json:"primary_window,omitempty"`
	SecondaryWindow            *codexTestWindow  `json:"secondary_window,omitempty"`
	Usage                      *codexTestUsage   `json:"usage,omitempty"`
	ResponseHeaders            []codexTestHeader `json:"response_headers,omitempty"`
	ResponseBody               string            `json:"response_body,omitempty"`
	BodyTruncated              bool              `json:"body_truncated,omitempty"`
}

// codexTestCapture keeps a side preview of the upstream body; past the limit it only adds a truncation marker and never truncates the stream that is actually being
// consumed by the parser.
type codexTestCapture struct {
	bytes.Buffer
	limit     int
	truncated bool
}

func (b *codexTestCapture) Write(p []byte) (int, error) {
	n := len(p)
	remaining := max(0, b.limit-b.Len())
	if len(p) > remaining {
		b.truncated = true
		p = p[:remaining]
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}

var codexTestProxyCredentials = regexp.MustCompile(`(?i)((?:https?|socks5h?)://)[^\s/]+@`)

// codexTestSecrets collects the account credentials that may appear in this probe's response body or headers, for redaction.
func codexTestSecrets(account *auth.Account) []string {
	if account == nil {
		return nil
	}
	secrets := make([]string, 0, 2)
	if token := account.GetAccessToken(); token != "" {
		secrets = append(secrets, token)
	}
	account.Mu().RLock()
	apiKey := strings.TrimSpace(account.APIKey)
	account.Mu().RUnlock()
	if apiKey != "" {
		secrets = append(secrets, apiKey)
	}
	return secrets
}

func sanitizeCodexTestText(text string, secrets []string) string {
	for _, secret := range secrets {
		if secret != "" {
			if len(secret) >= 8 {
				text = strings.ReplaceAll(text, secret, "[REDACTED]")
			} else {
				text = redactCodexTestShortSecret(text, secret)
			}
		}
	}
	text = codexTestProxyCredentials.ReplaceAllString(text, "${1}[REDACTED]@")
	return promptfilter.RedactSensitive(text)
}

// Short relay placeholders must not replace digits inside IDs or timestamps.
// Exact standalone occurrences still need masking; skipping short secrets could
// expose real credentials echoed by a provider.
func redactCodexTestShortSecret(text, secret string) string {
	var out strings.Builder
	tokenChar := func(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' || r == '-' }
	for offset := 0; ; {
		found := strings.Index(text[offset:], secret)
		if found < 0 {
			out.WriteString(text[offset:])
			break
		}
		start, end := offset+found, offset+found+len(secret)
		before, _ := utf8.DecodeLastRuneInString(text[:start])
		after, _ := utf8.DecodeRuneInString(text[end:])
		out.WriteString(text[offset:start])
		if (start == 0 || !tokenChar(before)) && (end == len(text) || !tokenChar(after)) {
			out.WriteString("[REDACTED]")
		} else {
			out.WriteString(secret)
		}
		offset = end
	}
	return out.String()
}

type codexTestRecorder struct {
	details *codexTestDiagnostics
	start   time.Time
	secrets []string
	capture codexTestCapture
	account *auth.Account
}

func newCodexTestRecorder(resp *http.Response, model string, account *auth.Account, start time.Time) *codexTestRecorder {
	secrets := codexTestSecrets(account)
	lookahead := 0
	for _, secret := range secrets {
		lookahead = max(lookahead, len(secret))
	}
	r := &codexTestRecorder{
		details: &codexTestDiagnostics{Model: model},
		start:   start,
		secrets: secrets,
		account: account,
		// Keep a little extra, so a credential that crosses the preview boundary can still be replaced in full.
		capture: codexTestCapture{limit: codexTestBodyLimit + lookahead},
	}
	if resp == nil {
		return r
	}
	r.details.HTTPStatus = resp.StatusCode
	ms := max(int64(0), time.Since(start).Milliseconds())
	r.details.Transport = codexTestTransport(resp.Header)
	if r.details.Transport != "websocket" {
		r.details.HeadersMS = &ms
		r.details.RequestID = r.safeValue(codexTestRequestID(resp.Header, account))
		r.details.CFRay = r.safeValue(resp.Header.Get("cf-ray"))
		r.details.PlanType = r.safeValue(resp.Header.Get("x-codex-plan-type"))
		r.details.PrimaryWindow = parseCodexTestWindowHeaders(resp.Header, "x-codex-primary-")
		r.details.SecondaryWindow = parseCodexTestWindowHeaders(resp.Header, "x-codex-secondary-")
		r.observeSafetyBufferingHeaders(resp.Header)
		r.appendHeaders(resp.Header)
	}
	if resp.Body != nil {
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.TeeReader(resp.Body, &r.capture), resp.Body}
	}
	return r
}

// codexTestTransport distinguishes a direct HTTP connection from forced WebSocket: the response headers synthesized on the WS path are 101
// handshake headers with Upgrade/Sec-WebSocket-Accept, and then the x-codex-* usage appears as codex.rate_limits
// frames rather than as response headers.
func codexTestTransport(header http.Header) string {
	if strings.EqualFold(strings.TrimSpace(header.Get("Upgrade")), "websocket") || header.Get("Sec-Websocket-Accept") != "" {
		return "websocket"
	}
	return "http"
}

// codexTestRequestID prefers the account's custom upstream request ID header, then falls back to the common names.
func codexTestRequestID(header http.Header, account *auth.Account) string {
	if override := strings.TrimSpace(account.GetUpstreamRequestIDHeader()); override != "" && auth.ValidateUpstreamRequestIDHeader(override) == nil {
		if value := strings.TrimSpace(header.Get(override)); value != "" {
			return value
		}
	}
	for _, name := range []string{"x-request-id", "request-id", "x-openai-request-id", "x-oai-request-id", "x-goog-request-id"} {
		if value := strings.TrimSpace(header.Get(name)); value != "" {
			return value
		}
	}
	return ""
}

// observeSafetyBufferingHeaders reads the x-codex-safety-buffering-* headers; if they are absent, the value stays unset.
func (r *codexTestRecorder) observeSafetyBufferingHeaders(header http.Header) {
	if raw := strings.TrimSpace(header.Get("x-codex-safety-buffering-enabled")); raw != "" {
		if enabled, err := strconv.ParseBool(raw); err == nil {
			r.details.SafetyBufferingEnabled = &enabled
		}
	}
	if model := strings.TrimSpace(header.Get("x-codex-safety-buffering-faster-model")); model != "" {
		r.details.SafetyBufferingFasterModel = r.safeValue(model)
	}
}

func (r *codexTestRecorder) appendHeaders(header http.Header) {
	keys := make([]string, 0, len(header))
	for key := range header {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		name := strings.ToLower(key)
		if !codexTestHeaderAllowed(name) {
			continue
		}
		value := r.safeValue(strings.Join(header[key], ", "))
		replaced := false
		for i := range r.details.ResponseHeaders {
			if r.details.ResponseHeaders[i].Name == name {
				r.details.ResponseHeaders[i].Value = value
				replaced = true
				break
			}
		}
		if !replaced && len(r.details.ResponseHeaders) < 64 {
			r.details.ResponseHeaders = append(r.details.ResponseHeaders, codexTestHeader{Name: name, Value: value})
		}
	}
}

// codexTestHeaderAllowed only lets through response headers with clear diagnostic value: usage windows, rate limits, request IDs,
// timing and error summaries; cookie and auth headers never enter the allowlist.
func codexTestHeaderAllowed(name string) bool {
	for _, prefix := range []string{"x-codex-", "x-ratelimit-", "openai-"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	switch name {
	case "request-id", "x-request-id", "x-openai-request-id", "x-oai-request-id", "x-goog-request-id",
		"retry-after", "content-type", "date", "server-timing", "cf-ray", "cf-cache-status", "x-should-retry",
		"x-error-code", "x-error-message", "www-authenticate", "upgrade":
		return true
	}
	return false
}

func parseCodexTestWindowHeaders(header http.Header, prefix string) *codexTestWindow {
	window := &codexTestWindow{
		UsedPercent:       parseCodexTestFloat(header.Get(prefix + "used-percent")),
		WindowMinutes:     parseCodexTestFloat(header.Get(prefix + "window-minutes")),
		ResetAfterSeconds: parseCodexTestFloat(header.Get(prefix + "reset-after-seconds")),
	}
	if window.empty() {
		return nil
	}
	return window
}

func parseCodexTestFloat(raw string) *float64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || value < 0 {
		return nil
	}
	return &value
}

func (r *codexTestRecorder) safeValue(value string) string {
	return truncate(sanitizeCodexTestText(value, r.secrets), 2048)
}

func (r *codexTestRecorder) contentReceived() {
	if r.details.FirstContentMS == nil {
		ms := max(int64(0), time.Since(r.start).Milliseconds())
		r.details.FirstContentMS = &ms
	}
}

// observe extracts the response ID from one Responses SSE event (or from the JSON error body of a non-200 response),
// the final state, the error type, and the usage. The event usage is the full final value, so it overwrites rather than adds up.
func (r *codexTestRecorder) observe(data []byte) {
	event := gjson.ParseBytes(data)
	if !event.IsObject() {
		return
	}
	if r.details.Transport == "websocket" && r.details.FirstFrameMS == nil {
		ms := max(int64(0), time.Since(r.start).Milliseconds())
		r.details.FirstFrameMS = &ms
	}
	eventType := event.Get("type").String()
	switch eventType {
	case "codex.rate_limits":
		r.observeRateLimitsFrame(event)
		return
	case "codex.response.metadata", "response.metadata":
		r.observeMetadataFrame(event)
		return
	}
	if flag := event.Get("safety_buffering"); flag.Type == gjson.True {
		r.details.SafetyBuffered = true
	}
	response := event.Get("response")
	// Lifecycle fields (id/model/status) are only read from Responses objects: the response wrapper of streaming events,
	// or a non-streaming top-level object=response. The top-level status of a 4xx error body is a numeric HTTP code,
	// and an in-stream error frame may also carry a numeric status; neither may be mixed into response_status.
	lifecycle := response.IsObject()
	if !lifecycle {
		response = event
		lifecycle = event.Get("object").String() == "response"
	}
	if lifecycle {
		if id := response.Get("id"); id.Type == gjson.String && id.String() != "" {
			r.details.ResponseID = r.safeValue(id.String())
		}
		if model := response.Get("model"); model.Type == gjson.String && model.String() != "" {
			r.details.ResponseModel = r.safeValue(model.String())
		}
		if status := response.Get("status"); status.Type == gjson.String && status.String() != "" {
			r.details.ResponseStatus = r.safeValue(status.String())
		}
		if reason := response.Get("incomplete_details.reason").String(); reason != "" {
			r.details.IncompleteReason = r.safeValue(reason)
		}
	}
	for _, candidate := range []gjson.Result{
		event.Get("error"),
		response.Get("error"),
		response.Get("status_details.error"),
	} {
		if !candidate.IsObject() {
			continue
		}
		if typ := candidate.Get("type").String(); typ != "" {
			r.details.ErrorType = r.safeValue(typ)
		}
		if code := candidate.Get("code").String(); code != "" {
			r.details.ErrorCode = r.safeValue(code)
		}
	}
	// An in-stream error event puts code at the top level: {"type":"error","code":"...","message":"..."}.
	if eventType == "error" {
		if code := event.Get("code").String(); code != "" {
			r.details.ErrorCode = r.safeValue(code)
		}
	}
	if eventType == "response.output_text.delta" && event.Get("delta").String() != "" {
		r.contentReceived()
	}
	usage := response.Get("usage")
	if !lifecycle || !usage.IsObject() {
		return
	}
	if r.details.Usage == nil {
		r.details.Usage = &codexTestUsage{}
	}
	u := r.details.Usage
	updateCodexTestCount(&u.InputTokens, usage.Get("input_tokens"))
	updateCodexTestCount(&u.OutputTokens, usage.Get("output_tokens"))
	updateCodexTestCount(&u.TotalTokens, usage.Get("total_tokens"))
	updateCodexTestCount(&u.CachedTokens, usage.Get("input_tokens_details.cached_tokens"))
	updateCodexTestCount(&u.ReasoningTokens, usage.Get("output_tokens_details.reasoning_tokens"))
}

// observeRateLimitsFrame handles the codex.rate_limits frame that appears in place of response headers under the WebSocket transport.
func (r *codexTestRecorder) observeRateLimitsFrame(event gjson.Result) {
	if plan := firstNonEmptyGJSON(event, "plan_type", "rate_limits.plan_type"); plan != "" {
		r.details.PlanType = r.safeValue(plan)
	}
	if window := parseCodexTestWindowFrame(event.Get("rate_limits.primary")); window != nil {
		r.details.PrimaryWindow = window
	}
	if window := parseCodexTestWindowFrame(event.Get("rate_limits.secondary")); window != nil {
		r.details.SecondaryWindow = window
	}
}

// observeMetadataFrame merges the upstream headers in codex.response.metadata / response.metadata frames
// into the allowlisted header list (on the WS path, x-codex-* only appears here).
func (r *codexTestRecorder) observeMetadataFrame(event gjson.Result) {
	headers := event.Get("headers")
	if !headers.IsObject() {
		return
	}
	merged := make(http.Header)
	headers.ForEach(func(key, value gjson.Result) bool {
		if value.Type == gjson.String {
			merged.Add(key.String(), value.String())
		}
		return true
	})
	r.appendHeaders(merged)
	if id := codexTestRequestID(merged, r.account); id != "" {
		r.details.RequestID = r.safeValue(id)
	}
	if ray := merged.Get("cf-ray"); ray != "" {
		r.details.CFRay = r.safeValue(ray)
	}
	r.observeSafetyBufferingHeaders(merged)
	if r.details.PlanType == "" {
		r.details.PlanType = r.safeValue(merged.Get("x-codex-plan-type"))
	}
	if r.details.PrimaryWindow.empty() {
		r.details.PrimaryWindow = parseCodexTestWindowHeaders(merged, "x-codex-primary-")
	}
	if r.details.SecondaryWindow.empty() {
		r.details.SecondaryWindow = parseCodexTestWindowHeaders(merged, "x-codex-secondary-")
	}
}

func parseCodexTestWindowFrame(node gjson.Result) *codexTestWindow {
	if !node.IsObject() {
		return nil
	}
	window := &codexTestWindow{
		UsedPercent:   parseCodexTestNumber(node.Get("used_percent")),
		WindowMinutes: parseCodexTestNumber(node.Get("window_minutes")),
	}
	if reset := parseCodexTestNumber(node.Get("resets_in_seconds"), node.Get("reset_after_seconds")); reset != nil {
		window.ResetAfterSeconds = reset
	} else if at := node.Get("resets_at"); at.Type == gjson.Number {
		remaining := float64(time.Until(time.Unix(int64(at.Float()), 0)) / time.Second)
		if remaining >= 0 {
			window.ResetAfterSeconds = &remaining
		}
	}
	if window.empty() {
		return nil
	}
	return window
}

func parseCodexTestNumber(values ...gjson.Result) *float64 {
	for _, value := range values {
		if value.Type != gjson.Number || value.Float() < 0 {
			continue
		}
		n := value.Float()
		return &n
	}
	return nil
}

func firstNonEmptyGJSON(node gjson.Result, paths ...string) string {
	for _, path := range paths {
		if value := strings.TrimSpace(node.Get(path).String()); value != "" {
			return value
		}
	}
	return ""
}

func updateCodexTestCount(target **int64, value gjson.Result) {
	if value.Type != gjson.Number {
		return
	}
	n, err := strconv.ParseInt(value.Raw, 10, 64)
	if err == nil && n >= 0 {
		*target = &n
	}
}

func (r *codexTestRecorder) finish() *codexTestDiagnostics {
	ms := max(int64(0), time.Since(r.start).Milliseconds())
	r.details.DurationMS = &ms
	body := sanitizeCodexTestText(r.capture.String(), r.secrets)
	var pretty bytes.Buffer
	if json.Indent(&pretty, []byte(body), "", "  ") == nil {
		body = pretty.String()
	}
	r.details.BodyTruncated = r.capture.truncated || len(body) > codexTestBodyLimit
	if len(body) > codexTestBodyLimit {
		body = strings.ToValidUTF8(body[:codexTestBodyLimit], "")
	}
	r.details.ResponseBody = body
	return r.details
}
