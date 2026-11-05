package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/codex2api/auth"
)

func TestAccountBoundEgressHTTPRoutesAndPoolIsolation(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	var direct, bound, resin, inherited atomic.Int32
	server := func(label string, counter *atomic.Int32) *httptest.Server {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			counter.Add(1)
			if label != "resin" && r.Header.Get("X-Resin-Account") != "" {
				t.Error("unexpected Resin header")
			}
			io.WriteString(w, label)
		}))
		t.Cleanup(s.Close)
		return s
	}
	target := server("direct", &direct)
	boundProxy := server("bound", &bound)
	resinProxy := server("resin", &resin)
	envProxy := server("inherited", &inherited)
	previous := http.DefaultTransport
	transport := previous.(*http.Transport).Clone()
	envURL, _ := url.Parse(envProxy.URL)
	transport.Proxy = http.ProxyURL(envURL)
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = previous; transport.CloseIdleConnections() })
	withResin(t, &ResinConfig{BaseURL: resinProxy.URL, PlatformName: "test"})
	account := &auth.Account{DBID: 998101}
	request := func(ctx context.Context, want string) *http.Client {
		t.Helper()
		egress := ResolveCodexRequestEgress(ctx, account, target.URL, envProxy.URL, false)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, egress.URL, nil)
		egress.ApplyHeaders(req.Header)
		resp, err := egress.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if string(body) != want {
			t.Errorf("route = %q, want %q", body, want)
		}
		return egress.Client()
	}
	plain := request(context.Background(), "resin")
	detector := request(WithCodexDetectorRequest(context.Background()), "resin")
	directClient := request(WithAccountBoundEgress(context.Background(), ""), "direct")
	boundClient := request(WithAccountBoundEgress(context.Background(), boundProxy.URL), "bound")
	if directClient == plain || directClient == detector || directClient == boundClient || directClient == getPooledClient(account, "") {
		t.Error("explicit egress shared a business/Resin/different-route HTTP pool")
	}
	if again := request(WithAccountBoundEgress(context.Background(), ""), "direct"); again != directClient {
		t.Error("same route should reuse HTTP pool")
	}
	if direct.Load() != 2 || bound.Load() != 1 || resin.Load() != 2 || inherited.Load() != 0 {
		t.Errorf("captures direct=%d bound=%d resin=%d inherited=%d", direct.Load(), bound.Load(), resin.Load(), inherited.Load())
	}
}

func TestAccountBoundEgressInvalidProxyFailsClosed(t *testing.T) {
	withResin(t, nil)
	var received atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { received.Add(1) }))
	defer target.Close()
	for _, mode := range []string{"standard", "utls_chrome"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("CODEX_TRANSPORT_MODE", mode)
			for _, raw := range []string{"socks5h://", "ftp://127.0.0.1:9", "http://%"} {
				ctx := WithAccountBoundEgress(context.Background(), raw)
				egress := ResolveCodexRequestEgress(ctx, &auth.Account{DBID: 998102}, target.URL, raw, false)
				req, _ := http.NewRequestWithContext(ctx, http.MethodGet, egress.URL, nil)
				resp, err := egress.Client().Do(req)
				if resp != nil {
					resp.Body.Close()
				}
				if err == nil || !strings.Contains(err.Error(), "account-bound proxy") {
					t.Errorf("invalid proxy did not fail closed: %v", err)
				}
			}
		})
	}
	if received.Load() != 0 {
		t.Errorf("direct target received %d requests", received.Load())
	}
}


func TestAccountBoundEgressTransportModeAndAccountPools(t *testing.T) {
	account := &auth.Account{DBID: 998120}
	ctx := WithAccountBoundEgress(context.Background(), "")
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	standard := getRequestClient(ctx, account, "")
	if transport, ok := standard.Transport.(*http.Transport); !ok || transport.Proxy != nil {
		t.Fatal("direct standard TLS transport or route lost")
	}
	t.Setenv("CODEX_TRANSPORT_MODE", "utls_chrome")
	chrome := getRequestClient(ctx, account, "")
	if _, ok := chrome.Transport.(*utlsRoundTripper); !ok {
		t.Fatal("uTLS transport mode lost")
	}
	if chrome == standard || chrome == getRequestClient(ctx, &auth.Account{DBID: 998121}, "") {
		t.Fatal("TLS mode/account pool isolation lost")
	}
}

