package proxy

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const realtimeCallsTestSDP = "v=0\r\no=- 1 1 IN IP4 0.0.0.0\r\n"

// newRealtimeCallsTestHandler starts a fake ChatGPT Live create upstream and returns a handler wired to it.
// The upstream sends the JSON body it receives on the returned channel before it answers.
func newRealtimeCallsTestHandler(t *testing.T) (*Handler, chan []byte) {
	t.Helper()
	received := make(chan []byte, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- body
		w.Header().Set("Location", "/backend-api/codex/rtc_realtime_1")
		w.Header().Set("Content-Type", "application/sdp")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(realtimeCallsTestSDP))
	}))
	t.Cleanup(upstream.Close)
	previousURL := liveCallsURLForTest
	t.Cleanup(func() {
		liveCallsURLForTest = previousURL
	})
	liveCallsURLForTest = upstream.URL

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "at", PlanType: "plus", AccountID: "acct-live"})
	handler := NewHandler(store, nil, &config.Config{AdminSecret: "secret"}, nil)
	handler.SetRuntimeCache(cache.NewMemory(1))
	return handler, received
}

func takeRealtimeCallsUpstreamBody(t *testing.T, received chan []byte) []byte {
	t.Helper()
	select {
	case body := <-received:
		return body
	default:
		t.Fatal("the Live upstream was not called")
		return nil
	}
}

func assertRealtimeCallsAnswer(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/sdp") {
		t.Fatalf("Content-Type=%q, want application/sdp", rec.Header().Get("Content-Type"))
	}
	if !strings.HasPrefix(rec.Header().Get("Location"), "/v1/live/") {
		t.Fatalf("Location=%q, want /v1/live/<call_id>", rec.Header().Get("Location"))
	}
}

// assertRealtimeCallsUpstream checks the session that reached the Live upstream.
// OpenAI session fields such as output_modalities must not be forwarded, because the upstream speaks its own schema.
func assertRealtimeCallsUpstream(t *testing.T, body []byte, wantSDP, wantVoice, wantInstructions string) {
	t.Helper()
	if got := gjson.GetBytes(body, "sdp").String(); got != wantSDP {
		t.Fatalf("upstream sdp=%q, want %q", got, wantSDP)
	}
	if got := gjson.GetBytes(body, "session.model").String(); got != liveAudioDefaultModel {
		t.Fatalf("upstream model=%q, want %q", got, liveAudioDefaultModel)
	}
	if got := gjson.GetBytes(body, "session.audio.output.voice").String(); got != wantVoice {
		t.Fatalf("upstream voice=%q, want %q", got, wantVoice)
	}
	if got := gjson.GetBytes(body, "session.instructions").String(); got != wantInstructions {
		t.Fatalf("upstream instructions=%q, want %q", got, wantInstructions)
	}
	if got := gjson.GetBytes(body, "session.delegation.type").String(); got != "client" {
		t.Fatalf("upstream delegation.type=%q, want client", got)
	}
	if gjson.GetBytes(body, "session.output_modalities").Exists() || gjson.GetBytes(body, "session.type").Exists() {
		t.Fatalf("OpenAI session fields leaked to the Live upstream: %s", body)
	}
}

func TestRealtimeCallsMultipartMapsSession(t *testing.T) {
	handler, received := newRealtimeCallsTestHandler(t)
	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	if err := writer.WriteField("sdp", realtimeCallsTestSDP); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("session", `{"type":"realtime","model":"gpt-realtime","output_modalities":["audio"],"audio":{"output":{"voice":"alloy"}},"instructions":"Be brief."}`); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	c, rec := liveTestContext(t, http.MethodPost, "/v1/realtime/calls", form.Bytes(), &database.APIKeyRow{
		ID: 11, Limits: database.APIKeyLimits{AllowLive: true},
	})
	c.Request.Header.Set("Content-Type", writer.FormDataContentType())
	handler.RealtimeCalls(c)
	assertRealtimeCallsAnswer(t, rec)
	assertRealtimeCallsUpstream(t, takeRealtimeCallsUpstreamBody(t, received), realtimeCallsTestSDP, "cove", "Be brief.")
}

func TestRealtimeCallsJSONMapsVoiceAndDefaults(t *testing.T) {
	handler, received := newRealtimeCallsTestHandler(t)
	c, rec := liveTestContext(t, http.MethodPost, "/v1/realtime/calls", []byte(`{"sdp":"v=0\r\n","session":{"model":"gpt-live","voice":"shimmer"}}`), &database.APIKeyRow{
		ID: 11, Limits: database.APIKeyLimits{AllowLive: true},
	})
	handler.RealtimeCalls(c)
	assertRealtimeCallsAnswer(t, rec)
	assertRealtimeCallsUpstream(t, takeRealtimeCallsUpstreamBody(t, received), "v=0\r\n", "juniper", liveAudioDefaultInstructions)
}

func TestRealtimeCallsRawSDPBodies(t *testing.T) {
	cases := []struct {
		name        string
		path        string
		contentType string
	}{
		{name: "application/sdp with query model", path: "/v1/realtime/calls?model=gpt-realtime", contentType: "application/sdp"},
		{name: "text/plain on the unprefixed route", path: "/realtime/calls", contentType: "text/plain"},
		{name: "missing content type", path: "/realtime/calls", contentType: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler, received := newRealtimeCallsTestHandler(t)
			c, rec := liveTestContext(t, http.MethodPost, tc.path, []byte(realtimeCallsTestSDP), &database.APIKeyRow{
				ID: 11, Limits: database.APIKeyLimits{AllowLive: true},
			})
			if tc.contentType == "" {
				c.Request.Header.Del("Content-Type")
			} else {
				c.Request.Header.Set("Content-Type", tc.contentType)
			}
			handler.RealtimeCalls(c)
			assertRealtimeCallsAnswer(t, rec)
			assertRealtimeCallsUpstream(t, takeRealtimeCallsUpstreamBody(t, received), realtimeCallsTestSDP, "cove", liveAudioDefaultInstructions)
		})
	}
}

func TestRealtimeCallsRejectsBadRequests(t *testing.T) {
	cases := []struct {
		name        string
		path        string
		contentType string
		body        string
		wantMessage string
	}{
		{name: "missing sdp", path: "/v1/realtime/calls", contentType: "application/json", body: `{"session":{}}`, wantMessage: "sdp is required"},
		{name: "empty body", path: "/v1/realtime/calls", contentType: "application/json", body: ``, wantMessage: "sdp is required"},
		{name: "non-realtime model in session", path: "/v1/realtime/calls", contentType: "application/json", body: `{"sdp":"v=0","session":{"model":"gpt-4o"}}`, wantMessage: "model gpt-4o is not a realtime model; use gpt-realtime or gpt-live"},
		{name: "non-realtime model in query", path: "/v1/realtime/calls?model=gpt-4o-mini", contentType: "application/sdp", body: "v=0\r\n", wantMessage: "model gpt-4o-mini is not a realtime model"},
		{name: "session is not an object", path: "/v1/realtime/calls", contentType: "application/json", body: `{"sdp":"v=0","session":"nope"}`, wantMessage: "session must be a JSON object"},
		{name: "multipart session is not JSON", path: "/v1/realtime/calls", contentType: "multipart/form-data; boundary=x", body: "--x\r\nContent-Disposition: form-data; name=\"sdp\"\r\n\r\nv=0\r\n--x\r\nContent-Disposition: form-data; name=\"session\"\r\n\r\n{bad\r\n--x--\r\n", wantMessage: "session must be valid JSON"},
		{name: "body is not JSON", path: "/v1/realtime/calls", contentType: "application/json", body: `{"sdp":`, wantMessage: "request body must be valid JSON"},
		{name: "body too large", path: "/v1/realtime/calls", contentType: "application/sdp", body: strings.Repeat("a", liveUpstreamBodyLimit+1), wantMessage: "request body is too large"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := NewHandler(auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1}), nil, nil, nil)
			c, rec := liveTestContext(t, http.MethodPost, tc.path, []byte(tc.body), &database.APIKeyRow{
				ID: 11, Limits: database.APIKeyLimits{AllowLive: true},
			})
			c.Request.Header.Set("Content-Type", tc.contentType)
			handler.RealtimeCalls(c)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.wantMessage) {
				t.Fatalf("body=%s, want message %q", rec.Body.String(), tc.wantMessage)
			}
		})
	}
}

func TestRealtimeCallsRequiresAllowLive(t *testing.T) {
	handler := NewHandler(auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1}), nil, nil, nil)
	c, rec := liveTestContext(t, http.MethodPost, "/v1/realtime/calls", []byte(`{"sdp":"v=0","session":{}}`), &database.APIKeyRow{ID: 9})
	handler.RealtimeCalls(c)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRealtimeCallsRoutesRegistered(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := &Handler{}
	handler.RegisterRoutes(router)

	postRoutes := make(map[string]bool)
	for _, route := range router.Routes() {
		if route.Method == http.MethodPost {
			postRoutes[route.Path] = true
		}
	}
	for _, path := range []string{
		"/v1/realtime/calls",
		"/realtime/calls",
		"/v1/audio/transcriptions",
		"/audio/transcriptions",
	} {
		if !postRoutes[path] {
			t.Fatalf("expected POST route %s to be registered; routes=%v", path, postRoutes)
		}
	}
}

func TestAudioTranscriptionsNotImplemented(t *testing.T) {
	handler := &Handler{}
	c, rec := liveTestContext(t, http.MethodPost, "/v1/audio/transcriptions", []byte(`{}`), nil)
	handler.AudioTranscriptions(c)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "not_implemented") {
		t.Fatalf("body=%s", rec.Body.String())
	}
}
