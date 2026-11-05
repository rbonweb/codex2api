package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

const (
	codexAnalyticsEndpointDefault = "https://chatgpt.com/backend-api/codex/analytics-events/events"
	codexMetricsEndpointDefault   = "https://ab.chatgpt.com/otlp/v1/metrics"
	codexStatsigAPIKeyDefault     = "client-MkRuleRQBd6qakfnDYqJVR9JuXcY57Ljly3vi5JVUIO"
	codexTelemetryQueueSize       = 1024
	codexTelemetryWorkers         = 4
	codexTelemetryTimeout         = 10 * time.Second
	codexTelemetryDropLogInterval = time.Minute
	codexTelemetryStateTTL        = 5 * time.Minute
	codexTelemetryMaxEventBytes   = 8 << 20
)

var (
	codexAnalyticsEndpoint = codexAnalyticsEndpointDefault
	codexMetricsEndpoint   = codexMetricsEndpointDefault
	codexTelemetryRandIntN = rand.IntN
	codexTelemetryGlobal   = newCodexTelemetryManager()
)

type codexTelemetryClient struct {
	account     *auth.Account
	accessToken string
	accountID   string
	proxyURL    string
	userAgent   string
	originator  string
	version     string
}

type codexTelemetryProfile struct {
	client       codexTelemetryClient
	sessionID    string
	threadID     string
	turnID       string
	rootTurnID   string
	model        string
	effort       string
	serviceTier  string
	started      time.Time
	firstThread  bool
	dynamicTool  bool
	command      bool
	fileChange   bool
	turnMetadata gjson.Result
}

type codexTelemetryRequest struct {
	account       *auth.Account
	body          []byte
	sessionID     string
	proxyOverride string
	apiKey        string
	deviceCfg     *DeviceProfileConfig
	headers       http.Header
}

type codexTelemetryTerminal struct {
	status     string
	body       []byte
	firstEvent time.Time
	firstToken time.Time
}

type codexTelemetryAttempt struct {
	profile codexTelemetryProfile
	// mu protects firstEvent/firstToken: the stream-reading goroutine writes them in processEvent,
	// and when an upstream timeout or downstream disconnect happens, Close may trigger finish from another goroutine to read them.
	mu         sync.Mutex
	firstEvent time.Time
	firstToken time.Time
	done       sync.Once
	// Temporary timing probe: collected only when CODEX_TELEMETRY_TIMING_DEBUG=1, zero overhead otherwise.
	timing     bool
	parseNanos atomic.Int64
	eventCount atomic.Int64
}

type codexTelemetryJob struct {
	client  codexTelemetryClient
	url     string
	body    []byte
	metrics bool
}

type codexTelemetryManager struct {
	once    sync.Once
	queue   chan codexTelemetryJob
	mu      sync.Mutex
	threads map[string]time.Time
	metrics map[int64]*codexMetricState
	// Drops due to a full queue are summarized in logs only at intervals, to avoid per-item log spam under high traffic.
	dropped     atomic.Int64
	dropLogUnix atomic.Int64
}

// newCodexTelemetryManager creates the in-process telemetry queue and state container.
func newCodexTelemetryManager() *codexTelemetryManager {
	return &codexTelemetryManager{
		queue:   make(chan codexTelemetryJob, codexTelemetryQueueSize),
		threads: make(map[string]time.Time),
		metrics: make(map[int64]*codexMetricState),
	}
}

// start launches the send workers and the metrics flush loop on demand.
//
// Each job is a synchronous POST (possibly through an account proxy, with a timeout from a few hundred ms to 10s). A single serial worker
// would back up the queue at just a few QPS; use a small set of concurrent workers to handle normal traffic, and still drop when the queue is full.
func (m *codexTelemetryManager) start() {
	m.once.Do(func() {
		for range codexTelemetryWorkers {
			go m.worker()
		}
		go func() {
			ticker := time.NewTicker(time.Minute)
			defer ticker.Stop()
			for now := range ticker.C {
				m.flushMetrics(now)
			}
		}()
	})
}

// worker sends telemetry jobs from the queue serially.
func (m *codexTelemetryManager) worker() {
	for job := range m.queue {
		if err := sendCodexTelemetryJob(job); err != nil {
			log.Printf("Codex telemetry send failed: %v", err)
		}
	}
}

// enqueue adds a telemetry job without blocking; when the queue is full it drops the job directly and records a summary in the log at intervals.
func (m *codexTelemetryManager) enqueue(job codexTelemetryJob) {
	m.start()
	select {
	case m.queue <- job:
	default:
		dropped := m.dropped.Add(1)
		now := time.Now().Unix()
		last := m.dropLogUnix.Load()
		if now-last >= int64(codexTelemetryDropLogInterval/time.Second) && m.dropLogUnix.CompareAndSwap(last, now) {
			log.Printf("Codex telemetry queue is full, %d batches dropped in total", dropped)
		}
	}
}

// markThread records the thread observed for the account and reports whether it appears for the first time.
func (m *codexTelemetryManager) markThread(accountID int64, threadID string, now time.Time) bool {
	key := strconv.FormatInt(accountID, 10) + ":" + threadID
	m.mu.Lock()
	defer m.mu.Unlock()
	_, found := m.threads[key]
	// ponytail: a fixed cap is smaller than maintaining a second LRU; when it is exceeded, rebuild the whole thing.
	if len(m.threads) >= 4096 {
		m.threads = make(map[string]time.Time)
		found = false
	}
	m.threads[key] = now
	return !found
}

// codexStatsigAPIKey returns the environment override or the official public Statsig SDK key.
func codexStatsigAPIKey() string {
	if value := strings.TrimSpace(os.Getenv("CODEX_STATSIG_API_KEY")); value != "" {
		return value
	}
	return codexStatsigAPIKeyDefault
}

// codexTelemetryTimingDebug is the temporary timing probe switch: "Client telemetry timing probe" in the admin panel
// or the environment variable CODEX_TELEMETRY_TIMING_DEBUG=1 takes effect if either is turned on. When off, it collects no
// timing data, and behavior is exactly the same as without instrumentation; when on, it additionally prints [TELEMETRY-TIMING] logs for
// comparing request-entry and streaming-parse overhead with telemetry on and off.
func codexTelemetryTimingDebug() bool {
	if CurrentRuntimeSettings().CodexTelemetryTimingDebug {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CODEX_TELEMETRY_TIMING_DEBUG"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// codexTelemetryEnabled combines the runtime switch with the deployment-level off setting.
//
// The runtime switch defaults to off (an experimental feature, and the events are simulated, so the deployer decides whether they are sent out),
// so calling ExecuteRequest in tests does not send real tokens to the upstream, and there is no need to special-case by binary name.
func codexTelemetryEnabled() bool {
	if !CurrentRuntimeSettings().CodexTelemetryEnabled {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CODEX_TELEMETRY_ENABLED"))) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// codexTelemetryEligible decides whether a request should simulate official Codex telemetry.
func codexTelemetryEligible(body []byte, headers http.Header) bool {
	if !codexTelemetryEnabled() || headers == nil || !gjson.ValidBytes(body) {
		return false
	}
	if !strings.HasPrefix(CodexBaseURL, "https://chatgpt.com/backend-api/codex") {
		return false
	}
	if turnMetadataIndicatesCompaction(headers.Get(codexTurnMetadataHeader)) {
		return false
	}
	return !responsesBodyRequestsImageGeneration(body) && !requestBodyCompactionMeta(body).UsageTriggered
}

// beginCodexTelemetry creates an observation for eligible requests and sends the initialization data.
//
// Both the HTTP and WebSocket upstreams are covered: the WS executor wraps WebSocket frames as standard SSE
// `http.Response` (see wsrelay.websocketResponseToHTTP), so the SSE-based parsing in this chain
// applies equally to both transports and does not need to branch by transport.
func beginCodexTelemetry(input codexTelemetryRequest) *codexTelemetryAttempt {
	timing := codexTelemetryTimingDebug()
	var startedAt time.Time
	if timing {
		startedAt = time.Now()
	}
	if input.account == nil || !codexTelemetryEligible(input.body, input.headers) {
		return nil
	}
	client, ok := snapshotCodexTelemetryClient(input)
	if !ok {
		return nil
	}
	profile := buildCodexTelemetryProfile(client, input)
	profile.firstThread = codexTelemetryGlobal.markThread(input.account.ID(), profile.threadID, profile.started)
	profile.dynamicTool = codexTelemetryRandIntN(5) < 2
	profile.command = profile.dynamicTool && codexTelemetryRandIntN(2) == 0
	profile.fileChange = codexTelemetryRandIntN(5) == 0
	attempt := &codexTelemetryAttempt{profile: profile, timing: timing}
	codexTelemetryGlobal.enqueueAnalytics(codexInitializationEvents(profile))
	codexTelemetryGlobal.touchMetrics(profile)
	if timing {
		log.Printf("[TELEMETRY-TIMING] begin account=%d model=%s first_thread=%t setup_ms=%d",
			input.account.ID(), profile.model, profile.firstThread, time.Since(startedAt).Milliseconds())
	}
	return attempt
}

// snapshotCodexTelemetryClient freezes the account and client identity actually used by this request.
func snapshotCodexTelemetryClient(input codexTelemetryRequest) (codexTelemetryClient, bool) {
	client := codexTelemetryClient{
		account: input.account, accessToken: input.account.GetAccessToken(),
		accountID: input.account.EffectiveAccountID(), proxyURL: input.account.GetProxyURL(),
	}
	if client.accessToken == "" || client.accountID == "" || input.account.IsCodexAgentIdentity() {
		return codexTelemetryClient{}, false
	}
	if input.proxyOverride != "" {
		client.proxyURL = input.proxyOverride
	}
	identity, err := ResolveCodexOutboundClientIdentity(CodexClientIdentityInput{Account: input.account, APIKey: input.apiKey, DeviceConfig: input.deviceCfg, Headers: input.headers})
	if err != nil {
		return codexTelemetryClient{}, false
	}
	client.userAgent, client.version = identity.UserAgent, identity.Version
	client.originator = codexTelemetryOriginator(client.userAgent, input.headers)
	userAgentOverridden, originatorOverridden := false, false
	for name, value := range input.account.GetCustomHeaders() {
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "user-agent":
			client.userAgent = value
			userAgentOverridden = true
		case "originator":
			client.originator = value
			originatorOverridden = true
		case "chatgpt-account-id":
			client.accountID = value
		case "version":
			client.version = value
		}
	}
	if userAgentOverridden && !originatorOverridden {
		client.originator = CodexOriginatorForGeneratedUserAgent(client.userAgent)
	}
	return client, client.userAgent != "" && client.originator != ""
}

// codexTelemetryOriginator reuses a valid downstream value, or derives the originator from the User-Agent.
func codexTelemetryOriginator(userAgent string, headers http.Header) string {
	if value := strings.TrimSpace(headers.Get("Originator")); IsCodexOfficialClientByHeaders(userAgent, value) && value != "" {
		return value
	}
	return CodexOriginatorForGeneratedUserAgent(userAgent)
}

// codexAnalyticsIsolatedEvent is an event type that must be sent in a batch of its own. Real clients decide this in
// TrackEventRequest::should_send_in_isolated_request, which returns true only for accepted-line-fingerprints,
// meaning this event must occupy its own HTTP request.
func codexAnalyticsIsolatedEvent(eventType string) bool {
	return eventType == "codex_accepted_line_fingerprints"
}

// enqueueAnalytics encodes and queues analytics events following the real client's batching rules:
// isolated events (accepted-line-fingerprints) are batched on their own, and the other events are sent merged together.
func (m *codexTelemetryManager) enqueueAnalytics(events []codexAnalyticsEvent) {
	if len(events) == 0 {
		return
	}
	batch := make([]codexAnalyticsEvent, 0, len(events))
	flush := func() {
		if len(batch) == 0 {
			return
		}
		m.enqueueAnalyticsBatch(batch)
		batch = batch[:0]
	}
	for _, event := range events {
		if codexAnalyticsIsolatedEvent(event.EventType) {
			flush()
			m.enqueueAnalyticsBatch([]codexAnalyticsEvent{event})
			continue
		}
		batch = append(batch, event)
	}
	flush()
}

// enqueueAnalyticsBatch encodes a non-empty event batch and queues it.
func (m *codexTelemetryManager) enqueueAnalyticsBatch(events []codexAnalyticsEvent) {
	if len(events) == 0 {
		return
	}
	body, err := json.Marshal(map[string]any{"events": events})
	if err == nil {
		m.enqueue(codexTelemetryJob{client: events[0].client, url: codexAnalyticsEndpoint, body: body})
	}
}

// observeResult finishes the observation immediately or wraps the response stream, based on the upstream result.
func (a *codexTelemetryAttempt) observeResult(resp *http.Response, err error) {
	if a == nil {
		return
	}
	if err != nil || resp == nil {
		a.finish("failed", nil)
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || resp.Body == nil {
		a.finish("failed", nil)
		return
	}
	resp.Body = &codexTelemetryBody{ReadCloser: resp.Body, attempt: a}
}

// finish commits the terminal event and this round's metrics, only once.
func (a *codexTelemetryAttempt) finish(status string, terminal []byte) {
	if a == nil {
		return
	}
	a.done.Do(func() {
		a.mu.Lock()
		firstEvent, firstToken := a.firstEvent, a.firstToken
		a.mu.Unlock()
		result := codexTelemetryTerminal{status: status, body: terminal, firstEvent: firstEvent, firstToken: firstToken}
		codexTelemetryGlobal.enqueueAnalytics(codexTerminalEvents(a.profile, result))
		codexTelemetryGlobal.recordTurnMetrics(a.profile, result)
		if a.timing {
			log.Printf("[TELEMETRY-TIMING] finish account=%d status=%s parse_ms=%d events=%d",
				a.profile.client.account.ID(), status,
				a.parseNanos.Load()/int64(time.Millisecond), a.eventCount.Load())
		}
	})
}

type codexTelemetryBody struct {
	io.ReadCloser
	attempt  *codexTelemetryAttempt
	pending  []byte
	event    []byte
	dropping bool
}

func (b *codexTelemetryBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		if b.attempt.timing {
			startedAt := time.Now()
			b.observe(p[:n])
			b.attempt.parseNanos.Add(time.Since(startedAt).Nanoseconds())
		} else {
			b.observe(p[:n])
		}
	}
	if err == io.EOF {
		b.flushJSON()
		b.attempt.finish("interrupted", nil)
	}
	return n, err
}

func (b *codexTelemetryBody) Close() error {
	b.attempt.finish("interrupted", nil)
	return b.ReadCloser.Close()
}

func (b *codexTelemetryBody) observe(data []byte) {
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		part := data
		if i >= 0 {
			part = data[:i]
		}
		if !b.dropping && len(b.pending)+len(part) <= codexTelemetryMaxEventBytes {
			b.pending = append(b.pending, part...)
		} else {
			b.pending, b.event, b.dropping = nil, nil, true
		}
		if i < 0 {
			return
		}
		b.line(bytes.TrimSuffix(b.pending, []byte{'\r'}))
		if cap(b.pending) > 256<<10 {
			b.pending = nil
		} else {
			b.pending = b.pending[:0]
		}
		data = data[i+1:]
	}
}

func (b *codexTelemetryBody) line(line []byte) {
	if len(line) == 0 {
		b.processEvent(b.event)
		if cap(b.event) > 256<<10 {
			b.event = nil
		} else {
			b.event = b.event[:0]
		}
		b.dropping = false
		return
	}
	if bytes.HasPrefix(line, []byte("data:")) && !b.dropping {
		part := bytes.TrimSpace(line[5:])
		if len(b.event)+len(part) <= codexTelemetryMaxEventBytes {
			b.event = append(b.event, part...)
		}
	}
}

// processEvent parses the SSE events and records the first packet, first token, and terminal state.
func (b *codexTelemetryBody) processEvent(data []byte) {
	if !json.Valid(data) {
		return
	}
	now := time.Now()
	typ := gjson.GetBytes(data, "type").String()
	b.attempt.mu.Lock()
	if b.attempt.firstEvent.IsZero() {
		b.attempt.firstEvent = now
	}
	if b.attempt.firstToken.IsZero() && strings.HasSuffix(typ, ".delta") {
		b.attempt.firstToken = now
	}
	b.attempt.mu.Unlock()
	if b.attempt.timing {
		b.attempt.eventCount.Add(1)
	}
	switch typ {
	case "response.completed":
		b.attempt.finish("completed", data)
	case "response.failed", "error":
		b.attempt.finish("failed", data)
	case "response.incomplete":
		b.attempt.finish("interrupted", data)
	}
}

// flushJSON parses the final state when a non-streaming response ends.
func (b *codexTelemetryBody) flushJSON() {
	if len(b.event) > 0 {
		b.processEvent(b.event)
		return
	}
	if json.Valid(b.pending) {
		status := gjson.GetBytes(b.pending, "status").String()
		if status == "completed" {
			b.attempt.finish("completed", b.pending)
		} else if status == "failed" {
			b.attempt.finish("failed", b.pending)
		}
	}
}
