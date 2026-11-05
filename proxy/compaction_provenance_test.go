package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

func TestCompactionProvenanceTTL(t *testing.T) {
	t.Setenv(compactionAffinityTTLEnv, "36h")
	if got := compactionProvenanceTTL(); got != 36*time.Hour {
		t.Fatalf("compactionProvenanceTTL() = %v, want 36h", got)
	}

	t.Setenv(compactionAffinityTTLEnv, "invalid")
	if got := compactionProvenanceTTL(); got != defaultCompactionProvenanceTTL {
		t.Fatalf("invalid TTL = %v, want default %v", got, defaultCompactionProvenanceTTL)
	}
}

type compactionRuntimeTrackingCache struct {
	cache.TokenCache
	mu      sync.Mutex
	setTTLs []time.Duration
}

func (c *compactionRuntimeTrackingCache) SetRuntime(ctx context.Context, namespace, key string, value json.RawMessage, ttl time.Duration) error {
	c.mu.Lock()
	c.setTTLs = append(c.setTTLs, ttl)
	c.mu.Unlock()
	return c.TokenCache.SetRuntime(ctx, namespace, key, value, ttl)
}

func TestCompactionEncryptedContentsFromPayload(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    string
	}{
		{name: "output item done", payload: `{"type":"response.output_item.done","item":{"type":"compaction","encrypted_content":"event-state"}}`, want: "event-state"},
		{name: "completed response", payload: `{"type":"response.completed","response":{"output":[{"type":"compaction","encrypted_content":"completed-state"}]}}`, want: "completed-state"},
		{name: "compact json", payload: `{"output":[{"type":"context_compaction","encrypted_content":"json-state"}]}`, want: "json-state"},
		{name: "direct item", payload: `{"type":"compaction","encrypted_content":"direct-state"}`, want: "direct-state"},
		{name: "reasoning is excluded", payload: `{"output":[{"type":"reasoning","encrypted_content":"reasoning-state"}]}`, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := compactionEncryptedContentsFromPayload([]byte(tt.payload))
			if tt.want == "" {
				if len(got) != 0 {
					t.Fatalf("contents = %v, want none", got)
				}
				return
			}
			if len(got) != 1 || got[0] != tt.want {
				t.Fatalf("contents = %v, want [%q]", got, tt.want)
			}
		})
	}
}

func TestRecordCompactionProvenanceFromPayload(t *testing.T) {
	tokenCache := cache.NewMemory(1)
	handler := &Handler{cache: tokenCache}
	account := &auth.Account{DBID: 17, AccessToken: "native"}
	payload := []byte(`{"type":"response.output_item.done","item":{"type":"compaction","encrypted_content":"recorded-output"}}`)

	handler.recordCompactionProvenanceFromPayload(context.Background(), account, payload)
	_, ok, err := tokenCache.GetRuntime(context.Background(), compactionProvenanceCacheNamespace, compactionContentDigest("recorded-output"))
	if err != nil || !ok {
		t.Fatalf("recorded output lookup = ok %v, err %v", ok, err)
	}
}

// newCompactionAffinityUpstream starts one local Codex upstream (reached through the Resin forwarder)
// that dispatches on the Authorization bearer token, so each Codex account gets its own behaviour.
func newCompactionAffinityUpstream(t *testing.T, byToken map[string]http.HandlerFunc) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler, ok := byToken[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"unexpected bearer token"}}`)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	codexRestoredUseResin(t, server.URL)
}

func compactionAffinityStreamHandler(calls *atomic.Int32) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"compaction\",\"encrypted_content\":\"stream-output-state\"}}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_affinity\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
	}
}

func newCompactionAffinityCodexAccount(id int64, token string) *auth.Account {
	return &auth.Account{DBID: id, AccessToken: token, PlanType: "plus", AccountID: token}
}

func newCompactionAffinityStore(accounts ...*auth.Account) *auth.Store {
	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:      2,
		MaxRetries:          0,
		MaxRateLimitRetries: 0,
	})
	for _, account := range accounts {
		store.AddAccount(account)
	}
	return store
}

func runCompactionAffinityResponses(t *testing.T, handler *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	ginContext.Request = req
	handler.Responses(ginContext)
	return recorder
}

func TestResponsesRoutesKnownCompactionToProducerDomain(t *testing.T) {
	var otherCalls, producerCalls atomic.Int32
	newCompactionAffinityUpstream(t, map[string]http.HandlerFunc{
		"other":    compactionAffinityStreamHandler(&otherCalls),
		"producer": compactionAffinityStreamHandler(&producerCalls),
	})

	other := newCompactionAffinityCodexAccount(1, "other")
	producer := newCompactionAffinityCodexAccount(2, "producer")
	other.SetSchedulerPriority(100)
	store := newCompactionAffinityStore(other, producer)
	defer store.Stop()
	store.SetAffinityMode(auth.AffinityModeOff)
	handler := NewHandler(store, nil, nil, nil)
	handler.SetRuntimeCache(cache.NewMemory(1))
	if err := handler.recordCompactionProvenance(context.Background(), producer, "known-state"); err != nil {
		t.Fatal(err)
	}

	recorder := runCompactionAffinityResponses(t, handler, `{"model":"gpt-5.5","stream":true,"input":[{"type":"compaction","encrypted_content":"known-state"},{"role":"user","content":"continue"}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if producerCalls.Load() != 1 || otherCalls.Load() != 0 {
		t.Fatalf("producer calls = %d, other calls = %d", producerCalls.Load(), otherCalls.Load())
	}
	if _, ok, err := handler.cache.GetRuntime(context.Background(), compactionProvenanceCacheNamespace, compactionContentDigest("stream-output-state")); err != nil || !ok {
		t.Fatalf("stream output provenance = ok %v, err %v", ok, err)
	}
}

func TestResponsesPortableCompactionReturnsToNormalPoolScheduling(t *testing.T) {
	var otherCalls, producerCalls atomic.Int32
	var receivedBody []byte
	newCompactionAffinityUpstream(t, map[string]http.HandlerFunc{
		"other": func(w http.ResponseWriter, r *http.Request) {
			otherCalls.Add(1)
			receivedBody = readUpstreamRequestBody(r)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_portable\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
		},
		"producer": compactionAffinityStreamHandler(&producerCalls),
	})

	other := newCompactionAffinityCodexAccount(1, "other")
	producer := newCompactionAffinityCodexAccount(2, "producer")
	other.SetSchedulerPriority(100)
	store := newCompactionAffinityStore(other, producer)
	defer store.Stop()
	store.SetAffinityMode(auth.AffinityModeOff)
	handler := NewHandler(store, nil, nil, nil)
	handler.SetRuntimeCache(cache.NewMemory(1))
	if err := handler.recordCompactionProvenance(context.Background(), producer, portableCompactionFixture); err != nil {
		t.Fatal(err)
	}

	recorder := runCompactionAffinityResponses(t, handler, `{"model":"gpt-5.5","stream":true,"input":[{"type":"compaction","encrypted_content":"`+portableCompactionFixture+`"},{"role":"user","content":"continue"}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if otherCalls.Load() != 1 || producerCalls.Load() != 0 {
		t.Fatalf("normal scheduler should select higher-priority account: other calls = %d, producer calls = %d", otherCalls.Load(), producerCalls.Load())
	}
	if bytes.Contains(receivedBody, []byte(portableCompactionEnvelopePrefix)) {
		t.Fatalf("portable envelope reached upstream: %s", receivedBody)
	}
	if !bytes.Contains(receivedBody, []byte("portable context")) {
		t.Fatalf("decoded summary missing upstream: %s", receivedBody)
	}
}

func TestResponsesMixedPortableAndOpaqueCompactionRetainsOpaqueAffinity(t *testing.T) {
	var otherCalls, producerCalls atomic.Int32
	newCompactionAffinityUpstream(t, map[string]http.HandlerFunc{
		"other":    compactionAffinityStreamHandler(&otherCalls),
		"producer": compactionAffinityStreamHandler(&producerCalls),
	})

	other := newCompactionAffinityCodexAccount(1, "other")
	producer := newCompactionAffinityCodexAccount(2, "producer")
	other.SetSchedulerPriority(100)
	store := newCompactionAffinityStore(other, producer)
	defer store.Stop()
	store.SetAffinityMode(auth.AffinityModeOff)
	handler := NewHandler(store, nil, nil, nil)
	handler.SetRuntimeCache(cache.NewMemory(1))
	if err := handler.recordCompactionProvenance(context.Background(), producer, "known-opaque"); err != nil {
		t.Fatal(err)
	}

	recorder := runCompactionAffinityResponses(t, handler, `{"model":"gpt-5.5","stream":true,"input":[{"type":"compaction","encrypted_content":"`+portableCompactionFixture+`"},{"type":"compaction","encrypted_content":"known-opaque"}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if producerCalls.Load() != 1 || otherCalls.Load() != 0 {
		t.Fatalf("remaining opaque state must retain producer affinity: producer calls = %d, other calls = %d", producerCalls.Load(), otherCalls.Load())
	}
}

func TestResponsesPrefersProducerIndependentOfSessionAffinityMode(t *testing.T) {
	var seenAuthorization string
	completed := func(w http.ResponseWriter, r *http.Request) {
		seenAuthorization = r.Header.Get("Authorization")
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_preferred\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
	}
	newCompactionAffinityUpstream(t, map[string]http.HandlerFunc{"other": completed, "producer": completed})

	other := newCompactionAffinityCodexAccount(1, "other")
	producer := newCompactionAffinityCodexAccount(2, "producer")
	producer.HealthTier = auth.HealthTierWarm
	other.SetSchedulerPriority(100)
	store := newCompactionAffinityStore(other, producer)
	defer store.Stop()
	store.SetAffinityMode(auth.AffinityModeOff)
	handler := NewHandler(store, nil, nil, nil)
	handler.SetRuntimeCache(cache.NewMemory(1))
	if err := handler.recordCompactionProvenance(context.Background(), producer, "producer-state"); err != nil {
		t.Fatal(err)
	}

	recorder := runCompactionAffinityResponses(t, handler, `{"model":"gpt-5.5","stream":true,"input":[{"type":"compaction","encrypted_content":"producer-state"}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if seenAuthorization != "Bearer producer" {
		t.Fatalf("Authorization = %q, want producer credential", seenAuthorization)
	}
}

func TestResponsesNonStreamRoutesAndRecordsKnownCompaction(t *testing.T) {
	var otherCalls, producerCalls atomic.Int32
	jsonHandler := func(calls *atomic.Int32, outputState string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			// Codex always answers over SSE; the handler folds it into the non-stream JSON reply.
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp_nonstream","status":"completed","output":[{"type":"compaction","encrypted_content":"`+outputState+`"}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
		}
	}
	newCompactionAffinityUpstream(t, map[string]http.HandlerFunc{
		"other":    jsonHandler(&otherCalls, "wrong-output"),
		"producer": jsonHandler(&producerCalls, "nonstream-output-state"),
	})

	other := newCompactionAffinityCodexAccount(1, "other")
	producer := newCompactionAffinityCodexAccount(2, "producer")
	producer.HealthTier = auth.HealthTierWarm
	other.SetSchedulerPriority(100)
	store := newCompactionAffinityStore(other, producer)
	defer store.Stop()
	handler := NewHandler(store, nil, nil, nil)
	handler.SetRuntimeCache(cache.NewMemory(1))
	if err := handler.recordCompactionProvenance(context.Background(), producer, "nonstream-known-state"); err != nil {
		t.Fatal(err)
	}

	recorder := runCompactionAffinityResponses(t, handler, `{"model":"gpt-5.5","stream":false,"input":[{"type":"compaction","encrypted_content":"nonstream-known-state"}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if producerCalls.Load() != 1 || otherCalls.Load() != 0 {
		t.Fatalf("producer calls = %d, other calls = %d", producerCalls.Load(), otherCalls.Load())
	}
	if _, ok, err := handler.cache.GetRuntime(context.Background(), compactionProvenanceCacheNamespace, compactionContentDigest("nonstream-output-state")); err != nil || !ok {
		t.Fatalf("nonstream output provenance = ok %v, err %v", ok, err)
	}
}

func TestResponsesKnownCompactionAllowsSameDomainAccountFailover(t *testing.T) {
	var calls atomic.Int32
	newCompactionAffinityUpstream(t, map[string]http.HandlerFunc{
		"producer": compactionAffinityStreamHandler(&calls),
		"failover": compactionAffinityStreamHandler(&calls),
	})

	producer := newCompactionAffinityCodexAccount(1, "producer")
	producer.Status = auth.StatusError
	failover := newCompactionAffinityCodexAccount(2, "failover")
	store := newCompactionAffinityStore(producer, failover)
	defer store.Stop()
	handler := NewHandler(store, nil, nil, nil)
	handler.SetRuntimeCache(cache.NewMemory(1))
	if err := handler.recordCompactionProvenance(context.Background(), producer, "same-domain-state"); err != nil {
		t.Fatal(err)
	}

	recorder := runCompactionAffinityResponses(t, handler, `{"model":"gpt-5.5","stream":true,"input":[{"type":"compaction","encrypted_content":"same-domain-state"}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("same-domain failover calls = %d, want 1", calls.Load())
	}
}

// Kept for request_limits_test.go, which only needs a compact upstream that must never be reached.
func newCompactionAffinityCompactRelay(t *testing.T, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(compactionAffinityCompactHandler(calls))
}

func compactionAffinityCompactHandler(calls *atomic.Int32) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"cmp-affinity","object":"response.compaction","output":[{"type":"compaction","encrypted_content":"next-state"}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	}
}

func TestResponsesCompactRoutesKnownCompactionToProducerDomain(t *testing.T) {
	var otherCalls, producerCalls atomic.Int32
	newCompactionAffinityUpstream(t, map[string]http.HandlerFunc{
		"other":    compactionAffinityCompactHandler(&otherCalls),
		"producer": compactionAffinityCompactHandler(&producerCalls),
	})

	other := newCompactionAffinityCodexAccount(1, "other")
	producer := newCompactionAffinityCodexAccount(2, "producer")
	store := newCompactionAffinityStore(other, producer)
	defer store.Stop()
	handler := NewHandler(store, nil, nil, nil)
	handler.SetRuntimeCache(cache.NewMemory(1))
	if err := handler.recordCompactionProvenance(context.Background(), producer, "known-compact-state"); err != nil {
		t.Fatal(err)
	}

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewBufferString(`{"model":"gpt-5.5","input":[{"type":"compaction","encrypted_content":"known-compact-state"}]}`))
	req.Header.Set("Content-Type", "application/json")
	ginContext.Request = req
	handler.ResponsesCompact(ginContext)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if producerCalls.Load() != 1 || otherCalls.Load() != 0 {
		t.Fatalf("producer calls = %d, other calls = %d", producerCalls.Load(), otherCalls.Load())
	}
	if _, ok, err := handler.cache.GetRuntime(context.Background(), compactionProvenanceCacheNamespace, compactionContentDigest("next-state")); err != nil || !ok {
		t.Fatalf("compact output provenance = ok %v, err %v; response=%s", ok, err, recorder.Body.String())
	}
}

func TestResponsesCompactPortableCompactionReturnsToNormalPoolScheduling(t *testing.T) {
	var otherCalls, producerCalls atomic.Int32
	newCompactionAffinityUpstream(t, map[string]http.HandlerFunc{
		"other":    compactionAffinityCompactHandler(&otherCalls),
		"producer": compactionAffinityCompactHandler(&producerCalls),
	})

	other := newCompactionAffinityCodexAccount(1, "other")
	producer := newCompactionAffinityCodexAccount(2, "producer")
	other.SetSchedulerPriority(100)
	store := newCompactionAffinityStore(other, producer)
	defer store.Stop()
	store.SetAffinityMode(auth.AffinityModeOff)
	handler := NewHandler(store, nil, nil, nil)
	handler.SetRuntimeCache(cache.NewMemory(1))
	if err := handler.recordCompactionProvenance(context.Background(), producer, portableCompactionFixture); err != nil {
		t.Fatal(err)
	}

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewBufferString(`{"model":"gpt-5.5","input":[{"type":"compaction","encrypted_content":"`+portableCompactionFixture+`"}]}`))
	req.Header.Set("Content-Type", "application/json")
	ginContext.Request = req
	handler.ResponsesCompact(ginContext)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if otherCalls.Load() != 1 || producerCalls.Load() != 0 {
		t.Fatalf("compact normal scheduler should select higher-priority account: other calls = %d, producer calls = %d", otherCalls.Load(), producerCalls.Load())
	}
}

func TestResponsesWebSocketRoutesAndRecordsKnownCompaction(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousExec := WebsocketExecuteFunc
	t.Cleanup(func() { WebsocketExecuteFunc = previousExec })

	selectedAccount := make(chan int64, 1)
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		selectedAccount <- account.ID()
		sse := `data: {"type":"response.output_item.done","item":{"type":"compaction","encrypted_content":"ws-output-state"}}` + "\n\n" +
			`data: {"type":"response.completed","response":{"id":"resp_ws_affinity","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(sse))}, nil
	}

	first := &auth.Account{DBID: 1, AccessToken: "first", PlanType: "plus", AccountID: "first"}
	producer := &auth.Account{DBID: 2, AccessToken: "producer", PlanType: "plus", AccountID: "producer"}
	handler := NewHandler(newCompactionAffinityStore(first, producer), nil, &config.Config{AllowAnonymousV1: true}, nil)
	handler.SetRuntimeCache(cache.NewMemory(1))
	if err := handler.recordCompactionProvenance(context.Background(), producer, "ws-known-state"); err != nil {
		t.Fatal(err)
	}

	router := gin.New()
	handler.RegisterRoutes(router)
	server := httptest.NewServer(router)
	defer server.Close()
	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
	if err != nil {
		if response != nil {
			t.Fatalf("dial websocket: %v status=%d", err, response.StatusCode)
		}
		t.Fatal(err)
	}
	defer conn.Close()

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"model":"gpt-5.5","input":[{"type":"compaction","encrypted_content":"ws-known-state"}]}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case accountID := <-selectedAccount:
		if accountID != producer.ID() {
			t.Fatalf("selected account = %d, want producer %d", accountID, producer.ID())
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for upstream websocket request")
	}

	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	for range 2 {
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Fatalf("read websocket response: %v", err)
		}
	}
	if _, ok, err := handler.cache.GetRuntime(context.Background(), compactionProvenanceCacheNamespace, compactionContentDigest("ws-output-state")); err != nil || !ok {
		t.Fatalf("websocket output provenance = ok %v, err %v", ok, err)
	}
}

func TestResponsesWebSocketPortableCompactionReturnsToNormalPoolScheduling(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousExec := WebsocketExecuteFunc
	t.Cleanup(func() { WebsocketExecuteFunc = previousExec })

	type selection struct {
		accountID int64
		body      []byte
	}
	selected := make(chan selection, 1)
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		selected <- selection{accountID: account.ID(), body: append([]byte(nil), requestBody...)}
		sse := `data: {"type":"response.completed","response":{"id":"resp_ws_portable","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(sse))}, nil
	}

	other := &auth.Account{DBID: 1, AccessToken: "other", PlanType: "plus", AccountID: "other"}
	producer := &auth.Account{DBID: 2, AccessToken: "producer", PlanType: "plus", AccountID: "producer"}
	other.SetSchedulerPriority(100)
	store := newCompactionAffinityStore(other, producer)
	store.SetAffinityMode(auth.AffinityModeOff)
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	handler.SetRuntimeCache(cache.NewMemory(1))
	if err := handler.recordCompactionProvenance(context.Background(), producer, portableCompactionFixture); err != nil {
		t.Fatal(err)
	}

	router := gin.New()
	handler.RegisterRoutes(router)
	server := httptest.NewServer(router)
	defer server.Close()
	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
	if err != nil {
		if response != nil {
			t.Fatalf("dial websocket: %v status=%d", err, response.StatusCode)
		}
		t.Fatal(err)
	}
	defer conn.Close()

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"model":"gpt-5.5","input":[{"type":"compaction","encrypted_content":"`+portableCompactionFixture+`"}]}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-selected:
		if got.accountID != other.ID() {
			t.Fatalf("selected account = %d, want normal higher-priority account %d", got.accountID, other.ID())
		}
		if bytes.Contains(got.body, []byte(portableCompactionEnvelopePrefix)) || !bytes.Contains(got.body, []byte("portable context")) {
			t.Fatalf("portable compaction was not normalized before WebSocket dispatch: %s", got.body)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for upstream websocket request")
	}
}

type compactionFailingRuntimeCache struct {
	cache.TokenCache
}

func (c *compactionFailingRuntimeCache) GetRuntime(context.Context, string, string) (json.RawMessage, bool, error) {
	return nil, false, errors.New("cache unavailable")
}

func TestResponsesKeepsServingCompactionDuringCacheOutage(t *testing.T) {
	var calls atomic.Int32
	newCompactionAffinityUpstream(t, map[string]http.HandlerFunc{"key": compactionAffinityStreamHandler(&calls)})

	store := newCompactionAffinityStore(newCompactionAffinityCodexAccount(1, "key"))
	defer store.Stop()
	handler := NewHandler(store, nil, nil, nil)
	handler.SetRuntimeCache(&compactionFailingRuntimeCache{TokenCache: cache.NewMemory(1)})

	recorder := runCompactionAffinityResponses(t, handler, `{"model":"gpt-5.5","stream":true,"input":[{"type":"compaction","encrypted_content":"any-state"},{"role":"user","content":"continue"}]}`)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("normal scheduling calls = %d, want 1", calls.Load())
	}
}

func TestCompactionPayloadPrefilterSkipsNonCompactionBodies(t *testing.T) {
	if got := requestCompactionEncryptedContents([]byte(`{"input":[{"type":"reasoning","encrypted_content":"reasoning-state"}]}`)); len(got) != 0 {
		t.Fatalf("reasoning-only request produced contents: %v", got)
	}
	if got := compactionEncryptedContentsFromPayload([]byte(`{"type":"response.output_item.done","item":{"type":"reasoning","encrypted_content":"reasoning-state"}}`)); len(got) != 0 {
		t.Fatalf("reasoning-only frame produced contents: %v", got)
	}
	if got := requestCompactionEncryptedContents([]byte(`{"input":[{"type":"compaction","encrypted_content":"opaque"}]}`)); len(got) != 1 || got[0] != "opaque" {
		t.Fatalf("compaction request contents = %v", got)
	}
}

func TestResolveCompactionAffinityRefreshesRollingTTL(t *testing.T) {
	t.Setenv(compactionAffinityTTLEnv, "36h")
	tracking := &compactionRuntimeTrackingCache{TokenCache: cache.NewMemory(1)}
	handler := &Handler{cache: tracking}
	account := &auth.Account{DBID: 31, AccessToken: "native"}
	if err := handler.recordCompactionProvenance(context.Background(), account, "rolling-state"); err != nil {
		t.Fatal(err)
	}
	if _, err := handler.resolveCompactionAffinity(context.Background(), []byte(`{"input":[{"type":"compaction","encrypted_content":"rolling-state"}]}`)); err != nil {
		t.Fatal(err)
	}

	tracking.mu.Lock()
	ttls := append([]time.Duration(nil), tracking.setTTLs...)
	tracking.mu.Unlock()
	if len(ttls) != 2 || ttls[0] != 36*time.Hour || ttls[1] != 36*time.Hour {
		t.Fatalf("runtime Set TTLs = %v, want initial + refreshed 36h", ttls)
	}
}

func TestResolveCompactionAffinityFailsOpenOnCacheReadError(t *testing.T) {
	handler := &Handler{cache: &compactionFailingRuntimeCache{TokenCache: cache.NewMemory(1)}}

	resolution, err := handler.resolveCompactionAffinity(context.Background(), []byte(`{"input":[{"type":"compaction","encrypted_content":"any-state"}]}`))

	if err != nil {
		t.Fatalf("cache outage should not fail resolution: %v", err)
	}
	if resolution.Known {
		t.Fatalf("cache outage unexpectedly resolved provenance: %+v", resolution)
	}
}
