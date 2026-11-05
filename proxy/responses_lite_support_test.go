package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

// resetLearnedResponsesLiteSupport clears the learned table and registers a restore, to avoid cross-test contamination.
func resetLearnedResponsesLiteSupport(t *testing.T) {
	t.Helper()
	learnedResponsesLiteSupport.Lock()
	saved := learnedResponsesLiteSupport.bySlug
	learnedResponsesLiteSupport.bySlug = make(map[string]bool)
	learnedResponsesLiteSupport.Unlock()
	t.Cleanup(func() {
		learnedResponsesLiteSupport.Lock()
		learnedResponsesLiteSupport.bySlug = saved
		learnedResponsesLiteSupport.Unlock()
	})
}

func TestResponsesLiteSupportForModelBuiltinSeed(t *testing.T) {
	resetLearnedResponsesLiteSupport(t)

	tests := []struct {
		model        string
		wantSupports bool
		wantKnown    bool
	}{
		{model: "gpt-5.6-sol", wantSupports: true, wantKnown: true},
		{model: " GPT-5.6-TERRA ", wantSupports: true, wantKnown: true},
		{model: "codex-auto-review", wantSupports: true, wantKnown: true},
		{model: "gpt-5.5", wantSupports: false, wantKnown: true},
		{model: "gpt-5.4", wantSupports: false, wantKnown: true},
		{model: "gpt-5.7-nova", wantSupports: false, wantKnown: false},
		{model: "", wantSupports: false, wantKnown: false},
	}
	for _, tt := range tests {
		supports, known := responsesLiteSupportForModel(tt.model)
		if supports != tt.wantSupports || known != tt.wantKnown {
			t.Fatalf("responsesLiteSupportForModel(%q) = (%t,%t), want (%t,%t)",
				tt.model, supports, known, tt.wantSupports, tt.wantKnown)
		}
	}
}

func TestRecordResponsesLiteSupportFromManifest(t *testing.T) {
	resetLearnedResponsesLiteSupport(t)

	manifest := []byte(`{"models":[
		{"slug":"gpt-5.7-nova","use_responses_lite":true},
		{"slug":"gpt-5.6-sol","use_responses_lite":false},
		{"slug":"gpt-5.8-missing-flag"},
		{"slug":"gpt-5.8-bad-flag","use_responses_lite":"yes"},
		{"slug":"","use_responses_lite":true}
	]}`)
	RecordResponsesLiteSupportFromManifest(manifest)

	if supports, known := responsesLiteSupportForModel("gpt-5.7-nova"); !known || !supports {
		t.Fatalf("gpt-5.7-nova = (%t,%t), want learned true", supports, known)
	}
	// The manifest is the upstream source of truth; learned results override the builtin seed.
	if supports, known := responsesLiteSupportForModel("gpt-5.6-sol"); !known || supports {
		t.Fatalf("gpt-5.6-sol = (%t,%t), want learned override to false", supports, known)
	}
	// Entries whose field is missing or non-boolean stay unknown.
	if _, known := responsesLiteSupportForModel("gpt-5.8-missing-flag"); known {
		t.Fatalf("gpt-5.8-missing-flag should stay unknown")
	}
	if _, known := responsesLiteSupportForModel("gpt-5.8-bad-flag"); known {
		t.Fatalf("gpt-5.8-bad-flag should stay unknown")
	}

	// An invalid or empty manifest must not panic or change state.
	RecordResponsesLiteSupportFromManifest(nil)
	RecordResponsesLiteSupportFromManifest([]byte(`{"models":"nope"}`))
}

func TestGateResponsesLiteForModel(t *testing.T) {
	resetLearnedResponsesLiteSupport(t)

	tests := []struct {
		name      string
		requested bool
		body      []byte
		want      bool
	}{
		{name: "not requested stays off", requested: false, body: []byte(`{"model":"gpt-5.6-sol"}`), want: false},
		{name: "known non-lite model strips signal", requested: true, body: []byte(`{"model":"gpt-5.5"}`), want: false},
		{name: "known lite model keeps signal", requested: true, body: []byte(`{"model":"gpt-5.6-sol"}`), want: true},
		{name: "unknown model passes through", requested: true, body: []byte(`{"model":"gpt-5.7-nova"}`), want: true},
		{name: "missing model passes through", requested: true, body: []byte(`{}`), want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := gateResponsesLiteForModel(tt.requested, tt.body); got != tt.want {
				t.Fatalf("gateResponsesLiteForModel(%t, %s) = %t, want %t", tt.requested, tt.body, got, tt.want)
			}
		})
	}
}

// stubWebsocketExecute stubs out the WS executor and captures the frame body and handshake headers; t.Cleanup restores them automatically.
func stubWebsocketExecute(t *testing.T, capturedBody *[]byte, capturedHeaders *http.Header) {
	t.Helper()
	previousExec := WebsocketExecuteFunc
	t.Cleanup(func() { WebsocketExecuteFunc = previousExec })
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		*capturedBody = append([]byte(nil), requestBody...)
		if headers != nil {
			*capturedHeaders = headers.Clone()
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
	}
}

// Locks the position of the ExecuteRequest gate (the headline fix path, issue "gpt-5.5 + lite always 400"):
// when the downstream carries a lite signal and the model has already been rewritten by the entry mapping to a known non-lite model, the WS frame body must not carry
// the marker, and the HTTP request must not carry the lite header. If the gate is moved to before the payload rule rewrite, or dropped from either
// transport branch, this test fails.
func TestExecuteRequestGateStripsLiteOnBothTransports(t *testing.T) {
	resetLearnedResponsesLiteSupport(t)

	downstream := make(http.Header)
	downstream.Set(codexResponsesLiteHeader, "true")
	account := &auth.Account{DBID: 9, AccessToken: "at-9", AccountID: "acct-9"}

	// WS path
	var wsBody []byte
	wsHeaders := make(http.Header)
	stubWebsocketExecute(t, &wsBody, &wsHeaders)
	resp, err := ExecuteRequest(context.Background(), account, []byte(`{"model":"gpt-5.5","input":"hi"}`), "", "", "sk-test", nil, downstream, true)
	if err != nil {
		t.Fatalf("ExecuteRequest(ws) error = %v", err)
	}
	resp.Body.Close()
	if marker := gjson.GetBytes(wsBody, codexResponsesLiteWSMetadataPath); marker.Exists() {
		t.Fatalf("WS frame retained lite marker for non-lite model: %s", wsBody)
	}
	if got := wsHeaders.Get(codexResponsesLiteHeader); got != "" {
		t.Fatalf("WS handshake headers carried %s = %q, want stripped", codexResponsesLiteHeader, got)
	}

	// HTTP path (Resin reverse proxy pointed at the test server)
	liteHeader := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		liteHeader <- r.Header.Get(codexResponsesLiteHeader)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_test"}`))
	}))
	t.Cleanup(server.Close)
	SetResinConfig(&ResinConfig{BaseURL: server.URL, PlatformName: "test"})
	t.Cleanup(func() { SetResinConfig(nil) })

	resp, err = ExecuteRequest(context.Background(), account, []byte(`{"model":"gpt-5.5","input":"hi"}`), "", "", "sk-test", nil, downstream, false)
	if err != nil {
		t.Fatalf("ExecuteRequest(http) error = %v", err)
	}
	resp.Body.Close()
	if got := <-liteHeader; got != "" {
		t.Fatalf("HTTP upstream received %s = %q for non-lite model, want stripped", codexResponsesLiteHeader, got)
	}
}

// Locks in that the lite signature is captured after the payload rule rewrite: the WS marker that the rule injects for a lite-capable model
// must reach the upstream frame body. If the signature falls back to being captured before the rule, the injection would be silently deleted by prepare's cleanup branch.
func TestExecuteRequestPayloadRuleInjectedLiteMarkerSurvives(t *testing.T) {
	resetLearnedResponsesLiteSupport(t)
	withPayloadRules(t, `{"override":[{"models":["gpt-5.6-sol"],"params":{"client_metadata.ws_request_header_x_openai_internal_codex_responses_lite":"true"}}]}`)

	var wsBody []byte
	wsHeaders := make(http.Header)
	stubWebsocketExecute(t, &wsBody, &wsHeaders)

	account := &auth.Account{DBID: 9, AccessToken: "at-9", AccountID: "acct-9"}
	resp, err := ExecuteRequest(context.Background(), account, []byte(`{"model":"gpt-5.6-sol","input":"hi"}`), "", "", "sk-test", nil, http.Header{}, true)
	if err != nil {
		t.Fatalf("ExecuteRequest(ws) error = %v", err)
	}
	resp.Body.Close()
	if got := gjson.GetBytes(wsBody, codexResponsesLiteWSMetadataPath).String(); got != "true" {
		t.Fatalf("rule-injected lite marker lost on WS frame: %s", wsBody)
	}
}
