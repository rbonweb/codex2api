package proxy

import (
	"net/http"
	"testing"

	"github.com/codex2api/auth"
)

func codexAccount() *auth.Account {
	// An account with an empty UpstreamType is an official Codex account.
	return &auth.Account{AccountID: "acc-1"}
}

func TestBuildCodexRoutingHint(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"fast normalizes to priority", `{"model":"gpt-5.6-codex","service_tier":"fast"}`, "model=gpt-5.6-codex;tier=priority"},
		{"priority kept", `{"model":"gpt-5.6-codex","service_tier":"priority"}`, "model=gpt-5.6-codex;tier=priority"},
		{"ultrafast kept", `{"model":"gpt-5.6-sol","service_tier":"ultrafast"}`, "model=gpt-5.6-sol;tier=ultrafast"},
		{"flex kept", `{"model":"gpt-5.6-codex","service_tier":"flex"}`, "model=gpt-5.6-codex;tier=flex"},
		{"default drops to model-only", `{"model":"gpt-5.6","service_tier":"default"}`, "model=gpt-5.6"},
		{"auto drops to model-only", `{"model":"gpt-5.6","service_tier":"auto"}`, "model=gpt-5.6"},
		{"scale drops to model-only", `{"model":"gpt-5.6","service_tier":"scale"}`, "model=gpt-5.6"},
		{"unknown tier drops to model-only", `{"model":"gpt-5.6","service_tier":"turbo"}`, "model=gpt-5.6"},
		{"missing tier is model-only", `{"model":"gpt-5.6"}`, "model=gpt-5.6"},
		{"uppercase tier normalized", `{"model":"gpt-5.6","service_tier":"PRIORITY"}`, "model=gpt-5.6;tier=priority"},
		{"missing model yields empty", `{"service_tier":"priority"}`, ""},
		{"model with semicolon rejected", `{"model":"a;b","service_tier":"flex"}`, ""},
		{"model with equals rejected", `{"model":"a=b"}`, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildCodexRoutingHint([]byte(tc.body)); got != tc.want {
				t.Fatalf("buildCodexRoutingHint(%s) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

func TestApplyCodexRoutingHintForCodexAndNilAccount(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-codex","service_tier":"fast"}`)

	// Official Codex account: synthesize the hint.
	h := http.Header{}
	ApplyCodexRoutingHint(h, codexAccount(), body)
	if got := h.Get("X-Codex-Routing-Hint"); got != "model=gpt-5.6-codex;tier=priority" {
		t.Fatalf("codex account hint = %q, want priority hint", got)
	}

	// nil account: do not send.
	h = http.Header{}
	ApplyCodexRoutingHint(h, nil, body)
	if got := h.Get("X-Codex-Routing-Hint"); got != "" {
		t.Fatalf("nil account hint = %q, want empty", got)
	}
}

func TestApplyCodexRoutingHintStripsSpoofedHeaders(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6","service_tier":"default"}`)

	// Downstream spoofs this header in any letter case; it must be stripped unconditionally and then rebuilt from the gateway value.
	h := http.Header{
		"x-codex-routing-hint": []string{"model=evil;tier=priority"},
		"X-Codex-Routing-Hint": []string{"model=evil2"},
	}
	ApplyCodexRoutingHint(h, codexAccount(), body)
	values := h.Values("X-Codex-Routing-Hint")
	if len(values) != 1 || values[0] != "model=gpt-5.6" {
		t.Fatalf("hint values = %v, want single gateway value [model=gpt-5.6]", values)
	}
}

// The hint must be taken from the "final outbound body": the upstream only accepts priority, so flex/auto/default/scale are
// stripped from the outbound body by sanitizeServiceTierForUpstream, so the hint must degrade to model-only along with it,
// and must not declare a tier that was never actually sent. This case chains the two steps of the real pipeline to pin down this semantics.
func TestRoutingHintFollowsSanitizedOutboundTier(t *testing.T) {
	cases := []struct {
		clientTier string
		want       string
	}{
		{"priority", "model=gpt-5.6-sol;tier=priority"},
		{"fast", "model=gpt-5.6-sol;tier=priority"},
		{"flex", "model=gpt-5.6-sol"},
		{"default", "model=gpt-5.6-sol"},
		{"auto", "model=gpt-5.6-sol"},
		{"scale", "model=gpt-5.6-sol"},
	}
	for _, tc := range cases {
		t.Run(tc.clientTier, func(t *testing.T) {
			body := []byte(`{"model":"gpt-5.6-sol","service_tier":"` + tc.clientTier + `"}`)
			outbound := sanitizeServiceTierForUpstream(body)
			if got := buildCodexRoutingHint(outbound); got != tc.want {
				t.Fatalf("client tier %q -> hint %q, want %q (outbound body: %s)", tc.clientTier, got, tc.want, outbound)
			}
		})
	}
}

func TestApplyCodexRoutingHintDisabledByEnv(t *testing.T) {
	t.Setenv("CODEX_DISABLE_ROUTING_HINT", "1")
	body := []byte(`{"model":"gpt-5.6-codex","service_tier":"fast"}`)

	// When turned off, it is not synthesized, but inbound spoofing is still stripped.
	h := http.Header{"x-codex-routing-hint": []string{"model=evil"}}
	ApplyCodexRoutingHint(h, codexAccount(), body)
	if got := h.Get("X-Codex-Routing-Hint"); got != "" {
		t.Fatalf("hint = %q, want empty when disabled", got)
	}
}
