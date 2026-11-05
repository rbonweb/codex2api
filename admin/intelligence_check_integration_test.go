package admin

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/codex2api/proxy/wsrelay"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

const intelligenceSuccess = `{"type":"response.completed","response":{"status":"completed","model":"gpt-5.4","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"21"}]}],"usage":{"input_tokens":4,"output_tokens":2}}}`

func TestIntelligenceActualCodexHTTPAndWebsocketIdentity(t *testing.T) {
	// The native target is fixed to chatgpt.com. Run this fixture in a child
	// process so its local TLS trust roots cannot affect other tests or business
	// transports. The bound CONNECT proxy below never connects outside localhost.
	if os.Getenv("CODEX_INTELLIGENCE_LOCAL_TLS_TEST") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestIntelligenceActualCodexHTTPAndWebsocketIdentity$", "-test.count=1", "-test.timeout=30s")
		cmd.Env = append(os.Environ(), "CODEX_INTELLIGENCE_LOCAL_TLS_TEST=1", "GODEBUG="+os.Getenv("GODEBUG")+",x509usefallbackroots=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("local TLS fixture: %v\n%s", err, output)
		}
		return
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "chatgpt.com", Organization: []string{"local test"}}, DNSNames: []string{"chatgpt.com"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	root, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(root)
	x509.SetFallbackRoots(roots)
	t.Setenv("GODEBUG", os.Getenv("GODEBUG")+",x509usefallbackroots=1")
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	t.Setenv("CODEX_WS_STATELESS_ONESHOT", "true")
	previous := proxy.CurrentRuntimeSettings()
	previousResin := proxy.GetResinConfig()
	previousWS := proxy.WebsocketExecuteFunc
	t.Cleanup(func() {
		proxy.ApplyRuntimeSettings(previous)
		proxy.SetResinConfig(previousResin)
		proxy.WebsocketExecuteFunc = previousWS
		wsrelay.ShutdownExecutor()
	})
	type capture struct {
		header http.Header
		body   []byte
		ws     bool
	}
	captured := make(chan capture, 4)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if websocket.IsWebSocketUpgrade(r) {
			conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Close()
			_, body, err := conn.ReadMessage()
			if err != nil {
				t.Error(err)
				return
			}
			captured <- capture{r.Header.Clone(), body, true}
			conn.WriteMessage(websocket.TextMessage, []byte(intelligenceSuccess))
			for {
				if _, _, err = conn.ReadMessage(); err != nil {
					return
				}
			}
		}
		body, _ := io.ReadAll(r.Body)
		captured <- capture{r.Header.Clone(), body, false}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", intelligenceSuccess)
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	server.StartTLS()
	defer server.Close()
	boundProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != "chatgpt.com:443" {
			t.Errorf("unexpected proxy target: %s %s", r.Method, r.Host)
			w.WriteHeader(502)
			return
		}
		upstream, err := net.Dial("tcp", server.Listener.Addr().String())
		if err != nil {
			t.Error(err)
			return
		}
		defer upstream.Close()
		downstream, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer downstream.Close()
		io.WriteString(downstream, "HTTP/1.1 200 Connection Established\r\n\r\n")
		go io.Copy(upstream, downstream)
		io.Copy(downstream, upstream)
	}))
	defer boundProxy.Close()
	proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: "http://127.0.0.1:1", PlatformName: "must-not-be-used"})
	proxy.WebsocketExecuteFunc = wsrelay.ExecuteRequestWebsocket
	store := auth.NewStore(nil, nil, nil)
	h := &Handler{store: store}
	account := &auth.Account{DBID: 778112, ProxyURL: boundProxy.URL, AccessToken: "local-fixture-only", Models: []string{"gpt-5.4"}, Status: auth.StatusReady, Timezone: "America/Los_Angeles"}
	store.AddAccount(account)
	settings := previous
	settings.CodexRequestCompression = false
	settings.CodexTelemetryEnabled = false
	settings.CodexUserAgentConfig = `{"client_kind":"codex-exec","client_version":"0.162.1","os_name":"Linux","arch":"x86_64"}`
	// A context supplied by an admin browser is intentionally not used by this API.
	prompt := "<environment_context>\n<timezone>Asia/Shanghai</timezone>\n</environment_context>\n21"
	for i := 0; i < 3; i++ {
		settings.CodexForceWebsocket = i == 2
		if i == 1 {
			settings.CodexUserAgentConfig = `{"client_kind":"codex-exec","client_version":"0.162.2","os_name":"Linux","arch":"x86_64"}`
		}
		if i == 2 {
			account.CustomHeaders = map[string]string{"User-Agent": "codex_exec/0.163.0 (Linux; x86_64)", "Originator": "codex_exec", "Version": "0.163.0"}
			account.CodexFingerprintMode = auth.CodexFingerprintModeSingleMachineMultiWindow
		}
		proxy.ApplyRuntimeSettings(settings)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		result := h.executeIntelligenceCheck(ctx, account, "gpt-5.4", "high", prompt)
		cancel()
		if !result.Complete || result.Error != "" {
			t.Fatalf("run%d: %+v", i, result)
		}
		got := <-captured
		if got.ws != (i == 2) || got.header.Get("Authorization") != "Bearer local-fixture-only" || got.header.Get("Cookie") != "" {
			t.Fatalf("wrong transport/identity: %+v", got)
		}
		expectedVersion := []string{"0.162.1", "0.162.2", "0.163.0"}[i]
		if !strings.Contains(got.header.Get("User-Agent"), expectedVersion) || got.header.Get("Version") != expectedVersion {
			t.Fatalf("actual identity not inherited: %v", got.header)
		}
		if i == 2 && got.header.Get("Originator") != "codex_exec" {
			t.Fatalf("custom originator lost: %v", got.header)
		}
		if !strings.Contains(gjson.GetBytes(got.body, "input.0.content.0.text").String(), "America/Los_Angeles") {
			t.Fatalf("timezone lost: %s", got.body)
		}
		if gjson.GetBytes(got.body, "model").String() != "gpt-5.4" || gjson.GetBytes(got.body, "reasoning.effort").String() != "high" {
			t.Fatalf("model/effort changed: %s", got.body)
		}
		if i == 2 && result.EffectiveContext["transport"] != "websocket" {
			t.Fatal("transport audit lost")
		}
	}
}
