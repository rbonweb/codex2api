package proxy

import (
	"net/http"
	"testing"

	"github.com/codex2api/auth"
)

func withResin(t *testing.T, cfg *ResinConfig) {
	t.Helper()
	old := resinCfg.Load()
	oldFlag := auth.ResinEgressEnabled()
	SetResinConfig(cfg)
	t.Cleanup(func() {
		resinCfg.Store(old)
		auth.SetResinEgressEnabled(oldFlag)
	})
}

func TestResolveCodexEgressPrecedence(t *testing.T) {
	const target = "https://chatgpt.com/backend-api/codex/responses"
	account := &auth.Account{DBID: 7}

	t.Run("resin overrides proxy chain", func(t *testing.T) {
		withResin(t, &ResinConfig{BaseURL: "http://127.0.0.1:2260/tok", PlatformName: "codex2api"})
		egress := ResolveCodexEgress(account, target, "http://pool:8080")
		if egress.Kind != CodexEgressResin || !egress.ViaResin() {
			t.Fatalf("kind = %q, want resin", egress.Kind)
		}
		if egress.URL != "http://127.0.0.1:2260/tok/codex2api/https/chatgpt.com/backend-api/codex/responses" {
			t.Fatalf("url = %q", egress.URL)
		}
		if egress.ProxyURL != "http://pool:8080" || egress.DialProxyURL != "" {
			t.Fatalf("proxy kept for audit only: proxy=%q dial=%q", egress.ProxyURL, egress.DialProxyURL)
		}
		if egress.Client() == nil {
			t.Fatal("resin client must not be nil")
		}
		h := http.Header{}
		egress.ApplyHeaders(h)
		if got := h.Get("X-Resin-Account"); got != "7" {
			t.Fatalf("X-Resin-Account = %q, want 7", got)
		}
	})

	t.Run("proxy when resin disabled", func(t *testing.T) {
		withResin(t, nil)
		egress := ResolveCodexEgress(account, target, " http://pool:8080 ")
		if egress.Kind != CodexEgressProxy || egress.URL != target {
			t.Fatalf("kind=%q url=%q", egress.Kind, egress.URL)
		}
		if egress.DialProxyURL != "http://pool:8080" {
			t.Fatalf("dial proxy = %q", egress.DialProxyURL)
		}
		h := http.Header{}
		egress.ApplyHeaders(h)
		if h.Get("X-Resin-Account") != "" {
			t.Fatal("non-resin egress must not inject X-Resin-Account")
		}
	})

	t.Run("direct when nothing configured", func(t *testing.T) {
		withResin(t, nil)
		egress := ResolveCodexEgress(account, target, "")
		if egress.Kind != CodexEgressDirect || egress.URL != target {
			t.Fatalf("kind=%q url=%q", egress.Kind, egress.URL)
		}
	})

	t.Run("resin never applies without account identity", func(t *testing.T) {
		withResin(t, &ResinConfig{BaseURL: "http://127.0.0.1:2260/tok", PlatformName: "codex2api"})
		egress := ResolveCodexEgress(nil, target, "http://pool:8080")
		if egress.Kind != CodexEgressProxy || egress.URL != target {
			t.Fatalf("nil account: kind=%q url=%q", egress.Kind, egress.URL)
		}
		if egress.Client() != nil {
			t.Fatal("nil account without resin has no pooled client")
		}
	})
}

func TestResolveCodexWebsocketEgress(t *testing.T) {
	const target = "wss://chatgpt.com/backend-api/codex/responses"
	account := &auth.Account{DBID: 3}

	withResin(t, &ResinConfig{BaseURL: "http://127.0.0.1:2260/tok", PlatformName: "codex2api"})
	egress := ResolveCodexWebsocketEgress(account, target, "socks5://pool:1080")
	if egress.Kind != CodexEgressResin || egress.DialProxyURL != "" {
		t.Fatalf("resin ws: kind=%q dial=%q", egress.Kind, egress.DialProxyURL)
	}
	if egress.URL != "ws://127.0.0.1:2260/tok/codex2api/https/chatgpt.com/backend-api/codex/responses" {
		t.Fatalf("resin ws url = %q", egress.URL)
	}

	if got := CodexDialProxyURL(account, "socks5://pool:1080"); got != "" {
		t.Fatalf("dial proxy under resin = %q, want empty", got)
	}

	SetResinConfig(nil)
	if got := CodexDialProxyURL(account, " socks5://pool:1080 "); got != "socks5://pool:1080" {
		t.Fatalf("dial proxy without resin = %q", got)
	}
	egress = ResolveCodexWebsocketEgress(account, target, "socks5://pool:1080")
	if egress.Kind != CodexEgressProxy || egress.URL != target || egress.DialProxyURL != "socks5://pool:1080" {
		t.Fatalf("proxy ws: kind=%q url=%q dial=%q", egress.Kind, egress.URL, egress.DialProxyURL)
	}
}

func TestSetResinConfigSyncsAuthEgressFlagAndSummary(t *testing.T) {
	withResin(t, nil)
	if auth.ResinEgressEnabled() {
		t.Fatal("flag should be off when resin is disabled")
	}
	if got := CurrentCodexEgressSummary(); got.Mode != "proxy_chain" || got.ResinEnabled {
		t.Fatalf("summary = %+v, want proxy_chain", got)
	}

	SetResinConfig(&ResinConfig{BaseURL: "http://127.0.0.1:2260/secret-token", PlatformName: "codex2api"})
	if !auth.ResinEgressEnabled() {
		t.Fatal("flag should follow resin enablement")
	}
	got := CurrentCodexEgressSummary()
	if got.Mode != "resin" || !got.ResinEnabled || got.ResinPlatformName != "codex2api" {
		t.Fatalf("summary = %+v", got)
	}
	if got.ResinEndpoint != "http://127.0.0.1:2260/***" {
		t.Fatalf("endpoint must hide the token: %q", got.ResinEndpoint)
	}

	// 只填一半等于禁用,标记必须跟着回落。
	SetResinConfig(&ResinConfig{BaseURL: "http://127.0.0.1:2260/secret-token"})
	if IsResinEnabled() || auth.ResinEgressEnabled() {
		t.Fatal("half-filled config must disable resin and clear the flag")
	}
}

func TestMaskResinBaseURL(t *testing.T) {
	cases := map[string]string{
		"":                                   "",
		"http://127.0.0.1:2260/secret-token": "http://127.0.0.1:2260/***",
		"http://127.0.0.1:2260/secret/":      "http://127.0.0.1:2260/***",
		"https://resin.example.com":          "https://resin.example.com",
		"https://resin.example.com/":         "https://resin.example.com",
		"not a url":                          "***",
	}
	for in, want := range cases {
		if got := MaskResinBaseURL(in); got != want {
			t.Errorf("MaskResinBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
}
