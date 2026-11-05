package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

const continuousRetryKeepaliveComment = downstreamSSEKeepaliveComment

var continuousRetryKeepaliveInterval = downstreamSSEKeepaliveInterval

type continuousRetryKeepalive interface {
	Activate()
	Active() bool
	Keepalive() error
}

type continuousRetryKeepaliveContextKey struct{}

type requestContinuousRetryKeepalive struct {
	mu       sync.Mutex
	active   bool
	disabled bool
	ctx      context.Context
	last     time.Time
	write    func() error
	cancel   context.CancelCauseFunc
}

// Activate starts the request-level keepalive window; repeated activation does not reset an existing window.
func (k *requestContinuousRetryKeepalive) Activate() {
	if k == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.disabled || (k.ctx != nil && k.ctx.Err() != nil) {
		return
	}
	if !k.active {
		k.active = true
		// Start timing once the request is eligible for downstream keepalive; later short waits share the same window,
		// and a full cycle is not restarted on every retry.
		k.last = time.Now()
	}
}

// SetEnabled saves the endpoint's keepalive switch; admission and retries cannot re-enable keepalive that was explicitly turned off.
// After keepalive is allowed again, it must still pass the quota admission check separately and then Activate.
func (k *requestContinuousRetryKeepalive) SetEnabled(enabled bool) {
	if k == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.disabled = !enabled
	if !enabled {
		k.active = false
	}
}

// activeLocked checks both the admission state and the downstream lifecycle; the caller must hold mu.
func (k *requestContinuousRetryKeepalive) activeLocked() bool {
	return k.active && !k.disabled && (k.ctx == nil || k.ctx.Err() == nil)
}

// Active reports whether request-level keepalive is currently active.
func (k *requestContinuousRetryKeepalive) Active() bool {
	if k == nil {
		return false
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.activeLocked()
}

// Keepalive writes one protocol keepalive when the heartbeat is due, and cancels the request if the write fails.
func (k *requestContinuousRetryKeepalive) Keepalive() error {
	if k == nil {
		return nil
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.activeLocked() || k.write == nil {
		return nil
	}
	if continuousRetryKeepaliveInterval <= 0 {
		return nil
	}
	if !k.last.IsZero() && time.Since(k.last) < continuousRetryKeepaliveInterval {
		return nil
	}
	if err := k.write(); err != nil {
		if k.cancel != nil {
			k.cancel(err)
		}
		return err
	}
	k.last = time.Now()
	return nil
}

// installContinuousRetrySSEKeepalive installs SSE comment keepalive for streaming endpoints; non-streaming endpoints use 102 instead.
func installContinuousRetrySSEKeepalive(c *gin.Context, stream bool, contentType string) func() {
	return installContinuousRetrySSEKeepaliveWithOptions(c, stream, continuousRetrySSEKeepaliveOptions{
		contentType: contentType,
		payload:     continuousRetryKeepaliveComment,
	})
}

// installContinuousRetryMessagesKeepalive uses the ping event that the Messages protocol requires.
func installContinuousRetryMessagesKeepalive(c *gin.Context, stream bool) func() {
	return installContinuousRetrySSEKeepaliveWithOptions(c, stream, continuousRetrySSEKeepaliveOptions{
		contentType: "text/event-stream; charset=utf-8",
		payload:     downstreamMessagesKeepaliveEvent,
	})
}

type continuousRetrySSEKeepaliveOptions struct {
	contentType string
	payload     string
}

// installContinuousRetrySSEKeepaliveWithOptions installs request keepalive with a specified content type and heartbeat payload.
func installContinuousRetrySSEKeepaliveWithOptions(c *gin.Context, stream bool, options continuousRetrySSEKeepaliveOptions) func() {
	if !stream {
		return installContinuousRetryHTTPInformationalKeepalive(c)
	}
	if c == nil || c.Request == nil || c.Writer == nil {
		return func() {}
	}
	responseWriter, ok := c.Writer.(http.ResponseWriter)
	if !ok {
		return func() {}
	}
	if _, ok := responseWriter.(http.Flusher); !ok {
		return func() {}
	}
	if options.contentType == "" {
		options.contentType = "text/event-stream"
	}
	if options.payload == "" {
		options.payload = continuousRetryKeepaliveComment
	}
	original := c.Request
	requestCtx, cancel := context.WithCancelCause(original.Context())
	keepalive := &requestContinuousRetryKeepalive{ctx: requestCtx, write: func() error {
		setSSEStreamHeaders(c, options.contentType)
		if !c.Writer.Written() {
			// Cloudflare still limits the final response after 102 to 125s; for long-lived keepalive
			// an SSE 200 must be established first, then protocol heartbeat frames are written continuously.
			c.Writer.WriteHeaderNow()
		}
		if _, err := io.WriteString(responseWriter, options.payload); err != nil {
			return err
		}
		if flusher, ok := responseWriter.(http.Flusher); ok {
			flusher.Flush()
		}
		return nil
	}, cancel: cancel}
	c.Request = original.WithContext(context.WithValue(requestCtx, continuousRetryKeepaliveContextKey{}, continuousRetryKeepalive(keepalive)))
	return func() {
		cancel(nil)
		c.Request = original
	}
}

// unwrapHTTPResponseWriter unwraps a ResponseWriter wrapped by middleware and reports whether it unwrapped anything.
func unwrapHTTPResponseWriter(writer http.ResponseWriter) (http.ResponseWriter, bool) {
	if writer == nil {
		return nil, false
	}
	for depth := 0; depth < 8; depth++ {
		unwrapper, ok := writer.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return writer, depth > 0
		}
		next := unwrapper.Unwrap()
		if next == nil {
			return nil, false
		}
		writer = next
	}
	return nil, false
}

// installContinuousRetryHTTPInformationalKeepalive installs a non-committing
// keepalive for ordinary JSON endpoints. HTTP 102 is sent through the
// unwrapped net/http writer so the final JSON status/body can still be chosen
// later; calling Gin's WriteHeader or Flush here would commit an accidental
// 200 response. Non-streaming JSON cannot carry SSE comments, so the standard HTTP 102
// Processing keepalive, while keeping the status code and body semantics of the final JSON.
func installContinuousRetryHTTPInformationalKeepalive(c *gin.Context) func() {
	if c == nil || c.Request == nil || c.Writer == nil || !c.Request.ProtoAtLeast(1, 1) {
		return func() {}
	}
	writer, ok := c.Writer.(http.ResponseWriter)
	if !ok {
		return func() {}
	}
	writer, unwrapped := unwrapHTTPResponseWriter(writer)
	if !unwrapped {
		return func() {}
	}
	original := c.Request
	requestCtx, cancel := context.WithCancelCause(original.Context())
	keepalive := &requestContinuousRetryKeepalive{
		ctx: requestCtx,
		write: func() error {
			// net/http flushes informational headers immediately. Do not call
			// Flush: it would implicitly send the final 200 status.
			writer.WriteHeader(http.StatusProcessing)
			return nil
		},
		cancel: cancel,
	}
	c.Request = original.WithContext(context.WithValue(requestCtx, continuousRetryKeepaliveContextKey{}, continuousRetryKeepalive(keepalive)))
	return func() {
		cancel(nil)
		c.Request = original
	}
}

func setSSEStreamHeaders(c *gin.Context, contentType string) {
	if c == nil {
		return
	}
	if contentType == "" {
		contentType = "text/event-stream"
	}
	c.Header("Content-Type", contentType)
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
}

func continuousRetryKeepaliveForContext(ctx context.Context) continuousRetryKeepalive {
	if ctx == nil {
		return nil
	}
	keepalive, _ := ctx.Value(continuousRetryKeepaliveContextKey{}).(continuousRetryKeepalive)
	return keepalive
}

func activateContinuousRetryKeepalive(ctx context.Context) {
	if apiKeyModelRequestAdmissionPending(ctx) {
		return
	}
	if keepalive := continuousRetryKeepaliveForContext(ctx); keepalive != nil {
		keepalive.Activate()
	}
}

func activateContinuousRetryKeepaliveForLimit(ctx context.Context, retryLimit int) {
	if retryLimit == -1 {
		activateContinuousRetryKeepalive(ctx)
	}
}

// continuousRetryKeepaliveActive reports whether request-level keepalive is active in the context.
func continuousRetryKeepaliveActive(ctx context.Context) bool {
	if keepalive := continuousRetryKeepaliveForContext(ctx); keepalive != nil {
		return keepalive.Active()
	}
	return false
}

// continuousRetryKeepaliveDelay computes the remaining wait until the next request-level heartbeat.
func continuousRetryKeepaliveDelay(keepalive continuousRetryKeepalive) time.Duration {
	if continuousRetryKeepaliveInterval <= 0 {
		return 0
	}
	requestKeepalive, ok := keepalive.(*requestContinuousRetryKeepalive)
	if !ok {
		return continuousRetryKeepaliveInterval
	}
	requestKeepalive.mu.Lock()
	defer requestKeepalive.mu.Unlock()
	if !requestKeepalive.activeLocked() || requestKeepalive.last.IsZero() {
		return continuousRetryKeepaliveInterval
	}
	delay := continuousRetryKeepaliveInterval - time.Since(requestKeepalive.last)
	if delay < 0 {
		return 0
	}
	return delay
}

func continuousRetryContextError(ctx context.Context) error {
	if ctx == nil {
		return context.Canceled
	}
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	return ctx.Err()
}

// stopContinuousRetryTimer stops the timer and drains any timer event that may already be queued.
func stopContinuousRetryTimer(timer *time.Timer) {
	if timer == nil || timer.Stop() {
		return
	}
	select {
	case <-timer.C:
	default:
	}
}

type continuousRetryHTTPResult struct {
	response *http.Response
	err      error
}

// executeHTTPWithContinuousRetryKeepalive keeps the downstream alive while a
// retry attempt is waiting for upstream response headers. The worker never
// touches the downstream writer; that remains owned by the handler goroutine.
func executeHTTPWithContinuousRetryKeepalive(ctx context.Context, execute func() (*http.Response, error)) (*http.Response, error) {
	keepalive := continuousRetryKeepaliveForContext(ctx)
	if execute == nil {
		return nil, errors.New("nil upstream executor")
	}
	if keepalive == nil || continuousRetryKeepaliveInterval <= 0 {
		return execute()
	}

	result := make(chan continuousRetryHTTPResult)
	abandoned := make(chan struct{})
	go func() {
		response, err := execute()
		select {
		case result <- continuousRetryHTTPResult{response: response, err: err}:
		case <-abandoned:
			if response != nil && response.Body != nil {
				_ = response.Body.Close()
			}
		}
	}()
	defer close(abandoned)

	for {
		timer := time.NewTimer(continuousRetryKeepaliveDelay(keepalive))
		select {
		case callResult := <-result:
			stopContinuousRetryTimer(timer)
			return callResult.response, callResult.err
		case <-timer.C:
			if keepalive.Active() {
				if err := keepalive.Keepalive(); err != nil {
					return nil, err
				}
			}
		case <-ctx.Done():
			stopContinuousRetryTimer(timer)
			return nil, continuousRetryContextError(ctx)
		}
	}
}

// runWithContinuousRetryKeepalive keeps writing request-level keepalive while a blocking operation runs.
// The caller goroutine still consumes the operation result; a heartbeat write failure or request cancellation is returned as an error.
func runWithContinuousRetryKeepalive[T any](ctx context.Context, operation func() T) (T, error) {
	var zero T
	if operation == nil {
		return zero, errors.New("nil keepalive operation")
	}
	keepalive := continuousRetryKeepaliveForContext(ctx)
	if keepalive == nil || !keepalive.Active() || continuousRetryKeepaliveInterval <= 0 {
		return operation(), nil
	}
	result := make(chan T, 1)
	go func() { result <- operation() }()
	for {
		delay := continuousRetryKeepaliveDelay(keepalive)
		if delay <= 0 {
			if err := keepalive.Keepalive(); err != nil {
				return <-result, err
			}
			delay = continuousRetryKeepaliveDelay(keepalive)
			if delay <= 0 {
				delay = continuousRetryKeepaliveInterval
			}
		}
		timer := time.NewTimer(delay)
		select {
		case value := <-result:
			stopContinuousRetryTimer(timer)
			if ctxErr := ctx.Err(); ctxErr != nil {
				return value, continuousRetryContextError(ctx)
			}
			return value, nil
		case <-timer.C:
			if err := keepalive.Keepalive(); err != nil {
				return <-result, err
			}
		case <-ctx.Done():
			stopContinuousRetryTimer(timer)
			return <-result, continuousRetryContextError(ctx)
		}
	}
}

type continuousRetryReadResult struct {
	data []byte
	err  error
}

// keepaliveWhileReading stops writing to a disconnected downstream, but keeps the independent upstream context's
// bounded catch-up read. Continuous retry uses the downstream context, and a write failure still ends the current attempt immediately.
func keepaliveWhileReading(ctx context.Context, keepalive continuousRetryKeepalive) error {
	err := keepalive.Keepalive()
	if err != nil && ctx.Err() == nil {
		if requestKeepalive, ok := keepalive.(*requestContinuousRetryKeepalive); ok &&
			requestKeepalive.ctx != nil && requestKeepalive.ctx.Err() != nil {
			return nil
		}
	}
	return err
}

// readAllWithContinuousRetryKeepalive keeps an active JSON retry alive while
// the upstream response body is still being read. The worker owns the read;
// the caller goroutine remains the sole downstream writer. The upstream JSON body
// read must also fall within the keepalive window, so that no long stretch is spent in the body-read stage with no bytes once continuous retry has begun.
func readAllWithContinuousRetryKeepalive(ctx context.Context, reader io.Reader) ([]byte, error) {
	keepalive := continuousRetryKeepaliveForContext(ctx)
	if keepalive == nil || !keepalive.Active() || continuousRetryKeepaliveInterval <= 0 {
		return io.ReadAll(reader)
	}
	result := make(chan continuousRetryReadResult, 1)
	go func() {
		data, err := io.ReadAll(reader)
		result <- continuousRetryReadResult{data: data, err: err}
	}()
	for {
		delay := continuousRetryKeepaliveDelay(keepalive)
		if delay <= 0 {
			if err := keepaliveWhileReading(ctx, keepalive); err != nil {
				return nil, err
			}
			delay = continuousRetryKeepaliveDelay(keepalive)
			if delay <= 0 {
				delay = continuousRetryKeepaliveInterval
			}
		}
		timer := time.NewTimer(delay)
		select {
		case callResult := <-result:
			stopContinuousRetryTimer(timer)
			return callResult.data, callResult.err
		case <-timer.C:
			if err := keepaliveWhileReading(ctx, keepalive); err != nil {
				return nil, err
			}
		case <-ctx.Done():
			stopContinuousRetryTimer(timer)
			return nil, continuousRetryContextError(ctx)
		}
	}
}

type continuousRetryStreamItem[T any] struct {
	value T
	ack   chan bool
}

// readStreamWithContinuousRetryKeepalive pumps upstream reads through the
// handler goroutine. That goroutine remains the sole downstream writer and can
// therefore emit heartbeats without racing real SSE or WebSocket output.
func readStreamWithContinuousRetryKeepalive[T any](ctx context.Context, read func(func(T) bool) error, callback func(T) bool) error {
	keepalive := continuousRetryKeepaliveForContext(ctx)
	if keepalive == nil || !keepalive.Active() || continuousRetryKeepaliveInterval <= 0 {
		return read(callback)
	}

	items := make(chan continuousRetryStreamItem[T])
	done := make(chan error, 1)
	stop := make(chan struct{})
	go func() {
		done <- read(func(value T) bool {
			ack := make(chan bool, 1)
			select {
			case items <- continuousRetryStreamItem[T]{value: value, ack: ack}:
			case <-stop:
				return false
			case <-ctx.Done():
				return false
			}
			select {
			case keepReading := <-ack:
				return keepReading
			case <-stop:
				return false
			case <-ctx.Done():
				return false
			}
		})
	}()
	defer close(stop)

	for {
		timer := time.NewTimer(continuousRetryKeepaliveDelay(keepalive))
		select {
		case item := <-items:
			stopContinuousRetryTimer(timer)
			keepReading := callback(item.value)
			item.ack <- keepReading
			if !keepReading {
				return <-done
			}
		case err := <-done:
			stopContinuousRetryTimer(timer)
			return err
		case <-timer.C:
			if err := keepaliveWhileReading(ctx, keepalive); err != nil {
				return err
			}
		case <-ctx.Done():
			stopContinuousRetryTimer(timer)
			return continuousRetryContextError(ctx)
		}
	}
}

type continuousRetrySSEEvent struct {
	event string
	data  []byte
}

func readSSEStreamWithContinuousRetryKeepalive(ctx context.Context, body io.Reader, callback func(event string, data []byte) bool) error {
	return readStreamWithContinuousRetryKeepalive(ctx, func(yield func(continuousRetrySSEEvent) bool) error {
		return ReadSSEStreamWithEvent(body, func(event string, data []byte) bool {
			return yield(continuousRetrySSEEvent{event: event, data: data})
		})
	}, func(item continuousRetrySSEEvent) bool {
		return callback(item.event, item.data)
	})
}

// waitWithContinuousRetryKeepalive waits in short chunks only after a request
// has entered the unlimited retry path. It keeps the write on the handler
// goroutine, so it never races normal stream output. A failed heartbeat is a
// downstream write failure and stops the retry immediately.
func waitWithContinuousRetryKeepalive(ctx context.Context, interval time.Duration) bool {
	if interval <= 0 {
		return true
	}
	keepalive := continuousRetryKeepaliveForContext(ctx)
	if keepalive == nil || continuousRetryKeepaliveInterval <= 0 {
		return waitForRetryInterval(ctx, interval)
	}
	activateContinuousRetryKeepalive(ctx)
	remaining := interval
	for remaining > 0 {
		step := continuousRetryKeepaliveDelay(keepalive)
		if step <= 0 {
			if err := keepalive.Keepalive(); err != nil {
				return false
			}
			// A disabled or custom heartbeat may not advance its deadline.
			// When the heartbeat has not advanced the deadline, it must really wait, to avoid the processing goroutine busy-waiting with zero delay.
			step = continuousRetryKeepaliveDelay(keepalive)
			if step <= 0 {
				step = continuousRetryKeepaliveInterval
			}
		}
		if step > remaining {
			step = remaining
		}
		if !waitForRetryInterval(ctx, step) {
			return false
		}
		remaining -= step
		if err := keepalive.Keepalive(); err != nil {
			return false
		}
	}
	return true
}

func waitForRetryInterval(ctx context.Context, interval time.Duration) bool {
	if interval <= 0 {
		return true
	}
	if ctx == nil {
		time.Sleep(interval)
		return true
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// retryKeepaliveCommitted reports that a retry heartbeat has already committed
// the streaming HTTP response. Callers must send a protocol event from that
// point on; appending a fresh JSON HTTP response would corrupt the stream.
func retryKeepaliveCommitted(c *gin.Context) bool {
	return c != nil && c.Writer != nil && c.Writer.Written()
}

func writeCommittedResponsesRetryError(c *gin.Context, message string) bool {
	if !retryKeepaliveCommitted(c) {
		if c != nil && c.Request != nil && !claimContinuousRetryTerminal(c, continuousRetryProtocolResponses) {
			return true
		}
		return false
	}
	timedOut := c.Request != nil && continuousRetryDeadlineExceeded(c.Request.Context())
	if !timedOut && c.Request != nil && !claimContinuousRetrySuccessContext(c.Request.Context()) {
		timedOut = true
	}
	if c.Request != nil && c.Request.Context().Err() != nil && !timedOut {
		return true
	}
	code := "upstream_error"
	if timedOut {
		c.Set(continuousRetryTimeoutWrittenKey, true)
		if failure, ok := continuousRetryLastFailure(c.Request.Context()); ok {
			writeContinuousRetryLastFailure(c, continuousRetryProtocolResponses, failure)
			return true
		}
		code = ErrorCodeUpstreamTimeout
		message = continuousRetryTimeoutMessage
	}
	payload, _ := json.Marshal(gin.H{
		"type": "response.failed",
		"response": gin.H{
			"created_at": time.Now().Unix(),
			"status":     "failed",
			"error":      gin.H{"message": message, "type": "upstream_error", "code": code},
		},
	})
	_, _ = c.Writer.WriteString("data: " + string(payload) + "\n\n")
	if flusher, ok := c.Writer.(http.Flusher); ok {
		flusher.Flush()
	}
	return true
}

func writeCommittedChatRetryError(c *gin.Context, message string) bool {
	if !retryKeepaliveCommitted(c) {
		if c != nil && c.Request != nil && !claimContinuousRetryTerminal(c, continuousRetryProtocolChat) {
			return true
		}
		return false
	}
	timedOut := c.Request != nil && continuousRetryDeadlineExceeded(c.Request.Context())
	if !timedOut && c.Request != nil && !claimContinuousRetrySuccessContext(c.Request.Context()) {
		timedOut = true
	}
	if c.Request != nil && c.Request.Context().Err() != nil && !timedOut {
		return true
	}
	code := ErrorCodeUpstreamStreamBreak
	if timedOut {
		c.Set(continuousRetryTimeoutWrittenKey, true)
		if failure, ok := continuousRetryLastFailure(c.Request.Context()); ok {
			writeContinuousRetryLastFailure(c, continuousRetryProtocolChat, failure)
			return true
		}
		code = ErrorCodeUpstreamTimeout
		message = continuousRetryTimeoutMessage
	}
	payload, _ := json.Marshal(gin.H{
		"error": gin.H{"message": message, "type": ErrorTypeUpstreamError, "code": code},
	})
	_, _ = c.Writer.WriteString("data: " + string(payload) + "\n\n")
	if flusher, ok := c.Writer.(http.Flusher); ok {
		flusher.Flush()
	}
	return true
}

// Replay can fail before a retry heartbeat commits HTTP. These helpers write a
// protocol terminal to the real writer in either state: normal JSON after SSE
// would corrupt the stream, while silence would look like success.
// Replay may fail before the retry heartbeat has committed HTTP; these helpers write the protocol terminal state to the real writer in both states
// so that plain JSON is not appended after SSE and a silent return does not create wrong error semantics.
func writeContinuousRetryLocalResponsesError(c *gin.Context) bool {
	if c == nil || c.Writer == nil {
		return false
	}
	if c.Request != nil && c.Request.Context().Err() != nil {
		return true
	}
	if !c.Writer.Written() {
		c.Status(http.StatusInternalServerError)
	}
	payload, _ := json.Marshal(gin.H{
		"type": "response.failed",
		"response": gin.H{
			"created_at": time.Now().Unix(),
			"status":     "failed",
			"error": gin.H{
				"message": continuousRetryLocalFailureMessage,
				"type":    "server_error",
				"code":    ErrorCodeInternalError,
			},
		},
	})
	_, _ = c.Writer.WriteString("data: " + string(payload) + "\n\n")
	if flusher, ok := c.Writer.(http.Flusher); ok {
		flusher.Flush()
	}
	return true
}

func writeContinuousRetryLocalChatError(c *gin.Context) bool {
	if c == nil || c.Writer == nil {
		return false
	}
	if c.Request != nil && c.Request.Context().Err() != nil {
		return true
	}
	if !c.Writer.Written() {
		c.Status(http.StatusInternalServerError)
	}
	payload, _ := json.Marshal(gin.H{
		"error": gin.H{
			"message": continuousRetryLocalFailureMessage,
			"type":    "server_error",
			"code":    ErrorCodeInternalError,
		},
	})
	_, _ = c.Writer.WriteString("data: " + string(payload) + "\n\n")
	if flusher, ok := c.Writer.(http.Flusher); ok {
		flusher.Flush()
	}
	return true
}

// abortContinuousRetryCommitFailure closes a winning attempt that could not
// be replayed to the downstream. Replay/storage/write failures are local
// proxy failures, not upstream failures: retrying would only duplicate a paid
// request and could turn a broken client or filesystem into an infinite loop.
func abortContinuousRetryCommitFailure(h *Handler, account *auth.Account, resp *http.Response, attempt *continuousRetryStreamAttempt) {
	if attempt != nil {
		_ = attempt.Close()
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if h != nil && h.store != nil && account != nil {
		h.store.Release(account)
	}
	log.Printf("continuous retry replay commit failed; request aborted")
}

func continuousRetryRequestErrorMessage(err error) string {
	var structured *Error
	if errors.As(err, &structured) && structured != nil && structured.Message != "" {
		return structured.Message
	}
	return "Upstream request failed"
}
