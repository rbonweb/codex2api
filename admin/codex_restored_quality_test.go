package admin

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// These tests restore the Codex OAuth coverage of the quality test and the intelligence check that used a relay account as a stand-in upstream.
// The Codex upstream is fixed to chatgpt.com, so each test runs in a child process whose trust roots accept a local certificate for chatgpt.com, and the account's bound proxy is a local CONNECT proxy that tunnels to a local TLS server.
// Nothing leaves localhost and the local trust root cannot affect other tests.

const (
	codexRestoredTLSChildEnv            = "CODEX_RESTORED_LOCAL_TLS_TEST"
	codexRestoredTLSIntelligenceSuccess = `{"type":"response.completed","response":{"status":"completed","model":"gpt-5.4","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"21"}]}],"usage":{"input_tokens":4,"output_tokens":2}}}`
)

var codexRestoredTLSCertificate struct {
	once sync.Once
	der  []byte
	key  *ecdsa.PrivateKey
	err  error
}

// codexRestoredTLSRunInChild re-runs the calling top-level test in a child process with the local trust root and reports whether the caller must return.
func codexRestoredTLSRunInChild(t *testing.T) bool {
	t.Helper()
	if os.Getenv(codexRestoredTLSChildEnv) == "1" {
		codexRestoredTLSCertificate.once.Do(func() {
			key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				codexRestoredTLSCertificate.err = err
				return
			}
			template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "chatgpt.com", Organization: []string{"local test"}}, DNSNames: []string{"chatgpt.com"},
				NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
				IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
				ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
			der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
			if err != nil {
				codexRestoredTLSCertificate.err = err
				return
			}
			root, err := x509.ParseCertificate(der)
			if err != nil {
				codexRestoredTLSCertificate.err = err
				return
			}
			roots := x509.NewCertPool()
			roots.AddCert(root)
			x509.SetFallbackRoots(roots)
			codexRestoredTLSCertificate.der, codexRestoredTLSCertificate.key = der, key
		})
		if codexRestoredTLSCertificate.err != nil {
			t.Fatal(codexRestoredTLSCertificate.err)
		}
		gin.SetMode(gin.TestMode)
		t.Setenv("CODEX_TRANSPORT_MODE", "standard")
		previous := proxy.CurrentRuntimeSettings()
		settings := previous
		settings.CodexRequestCompression = false
		settings.CodexTelemetryEnabled = false
		settings.CodexForceWebsocket = false
		proxy.ApplyRuntimeSettings(settings)
		t.Cleanup(func() { proxy.ApplyRuntimeSettings(previous) })
		return false
	}
	cmd := exec.Command(os.Args[0], "-test.run=^"+regexp.QuoteMeta(t.Name())+"$", "-test.count=1", "-test.v", "-test.timeout=60s")
	cmd.Env = append(os.Environ(), codexRestoredTLSChildEnv+"=1", "GODEBUG="+os.Getenv("GODEBUG")+",x509usefallbackroots=1")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("local TLS fixture: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "--- PASS: "+t.Name()+" ") {
		t.Fatalf("child did not run %s:\n%s", t.Name(), output)
	}
	return true
}

// codexRestoredTLSUpstream serves handler as the Codex upstream and returns the bound proxy URL that tunnels chatgpt.com:443 to it.
func codexRestoredTLSUpstream(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{codexRestoredTLSCertificate.der}, PrivateKey: codexRestoredTLSCertificate.key}}}
	server.StartTLS()
	t.Cleanup(server.Close)
	tunnel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != "chatgpt.com:443" {
			t.Errorf("unexpected proxy target: %s %s", r.Method, r.Host)
			w.WriteHeader(http.StatusBadGateway)
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
		// A tunnel must end both directions as soon as either side ends: when the client drops the connection the upstream has to see it close, which is how cancellation reaches the fake upstream.
		// Copying one way and leaving the other open forwards no FIN or reset, so the upstream request context never ends and the test waits on an event that never comes.
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); io.Copy(upstream, downstream); upstream.Close() }()
		go func() { defer wg.Done(); io.Copy(downstream, upstream); downstream.Close() }()
		wg.Wait()
	}))
	t.Cleanup(tunnel.Close)
	return tunnel.URL
}

func codexRestoredTLSAccount(id int64, proxyURL string) *auth.Account {
	return &auth.Account{
		DBID:        id,
		AccessToken: "sk-test-secret",
		ProxyURL:    proxyURL,
		Models:      []string{"gpt-5.5"},
		Status:      auth.StatusReady,
		HealthTier:  auth.HealthTierHealthy,
	}
}

// codexRestoredTLSHandler builds an admin handler whose only account (ID 42) is a Codex OAuth account reaching the given upstream.
func codexRestoredTLSHandler(t *testing.T, upstream http.HandlerFunc) (*Handler, *auth.Account) {
	t.Helper()
	account := codexRestoredTLSAccount(42, codexRestoredTLSUpstream(t, upstream))
	store := auth.NewStore(nil, nil, nil)
	store.AddAccount(account)
	return &Handler{store: store}, account
}

func codexRestoredTLSQualityRouter(h *Handler) *gin.Engine {
	router := gin.New()
	router.GET("/accounts/:id/quality-test/options", h.QualityTestOptions)
	router.POST("/accounts/:id/quality-test", h.QualityTest)
	return router
}

func codexRestoredTLSQualityRequest(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/accounts/42/quality-test", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func codexRestoredTLSDecodeEvents(t *testing.T, body string) []testEvent {
	t.Helper()
	var events []testEvent
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event testEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		events = append(events, event)
	}
	return events
}

func TestQualityTestPinsAccountAndPreservesPromptEffortAndHTML_CodexUpstream(t *testing.T) {
	if codexRestoredTLSRunInChild(t) {
		return
	}
	const html = "<!doctype html><html><body><svg>\n  <text>鹈鹕</text>\n</svg></body></html>"
	const prompt = "创建 HTML\n第二行必须保留\n{{random}}"
	var gotBody []byte
	var gotAuthorization string
	h, _ := codexRestoredTLSHandler(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotAuthorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		for _, text := range []string{"<!doctype html><html><body><svg>", "\n  ", "<text>鹈鹕</text>\n</svg></body></html>"} {
			event, _ := json.Marshal(map[string]any{"type": "response.output_text.delta", "delta": text})
			fmt.Fprintf(w, "data: %s\n\n", event)
		}
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"model\":\"actual-model\",\"usage\":{\"input_tokens\":24,\"output_tokens\":88,\"output_tokens_details\":{\"reasoning_tokens\":40}}}}\n\n")
	})
	// A second Codex account whose bound proxy is dead: the test must use exactly the selected account.
	h.store.AddAccount(codexRestoredTLSAccount(43, "http://127.0.0.1:1"))
	requestBody, _ := json.Marshal(qualityTestRequest{Model: "gpt-5.5", Prompt: prompt, ReasoningEffort: "high"})
	recorder := httptest.NewRecorder()
	codexRestoredTLSQualityRouter(h).ServeHTTP(recorder, codexRestoredTLSQualityRequest(string(requestBody)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if gotAuthorization != "Bearer sk-test-secret" || gjson.GetBytes(gotBody, "model").String() != "gpt-5.5" {
		t.Fatalf("request did not use exactly the selected account/model: auth=%q body=%s", gotAuthorization, gotBody)
	}
	if got := gjson.GetBytes(gotBody, "input.0.content.0.text").String(); got != prompt {
		t.Fatalf("multiline prompt altered: %q", got)
	}
	if gjson.GetBytes(gotBody, "reasoning.effort").String() != "high" || strings.Contains(string(gotBody), "Reply briefly") {
		t.Fatalf("wrong quality-test instructions or effort: %s", gotBody)
	}
	var output strings.Builder
	completed := false
	var diagnostics *codexTestDiagnostics
	for _, event := range codexRestoredTLSDecodeEvents(t, recorder.Body.String()) {
		if event.Type == "content" {
			output.WriteString(event.Text)
		}
		if event.Type == "test_complete" {
			completed = event.Success
		}
		if event.CodexDiagnostics != nil {
			diagnostics = event.CodexDiagnostics
		}
	}
	if output.String() != html || !completed {
		t.Fatalf("HTML corrupted or missing completion: output=%q completed=%v", output.String(), completed)
	}
	if diagnostics == nil || diagnostics.ResponseModel != "actual-model" || diagnostics.Usage == nil || diagnostics.Usage.ReasoningTokens == nil || *diagnostics.Usage.ReasoningTokens != 40 {
		t.Fatalf("missing final metrics: %+v", diagnostics)
	}
}

func TestQualityTestCancellationReachesSelectedUpstream_CodexUpstream(t *testing.T) {
	if codexRestoredTLSRunInChild(t) {
		return
	}
	started, cancelled := make(chan struct{}), make(chan struct{})
	var startOnce, cancelOnce sync.Once
	h, _ := codexRestoredTLSHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.created\"}\n\n")
		w.(http.Flusher).Flush()
		startOnce.Do(func() { close(started) })
		<-r.Context().Done()
		cancelOnce.Do(func() { close(cancelled) })
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		codexRestoredTLSQualityRouter(h).ServeHTTP(recorder, codexRestoredTLSQualityRequest(`{"model":"gpt-5.5","prompt":"HTML"}`).WithContext(ctx))
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("upstream did not start")
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(10 * time.Second):
		t.Fatal("cancellation did not reach upstream")
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("handler did not stop")
	}
	for _, event := range codexRestoredTLSDecodeEvents(t, recorder.Body.String()) {
		if event.Type == "test_complete" && event.Success {
			t.Fatal("cancelled stream reported success")
		}
	}
}

// codexRestoredTLSJobRig is a database-backed handler running quality-test jobs.
type codexRestoredTLSJobRig struct {
	db     *database.DB
	store  *auth.Store
	h      *Handler
	router *gin.Engine
}

func newCodexRestoredTLSJobRig(t *testing.T) *codexRestoredTLSJobRig {
	t.Helper()
	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, nil)
	h := &Handler{db: db, store: store}
	parent, stop := context.WithCancel(context.Background())
	h.StartQualityTests(parent)
	t.Cleanup(func() { stop(); h.WaitQualityTests() })
	router := gin.New()
	router.POST("/accounts/:id/quality-test", h.CreateQualityTestJob)
	router.POST("/quality-tests/:id/cancel", (&Handler{db: db, store: store}).CancelQualityTest)
	return &codexRestoredTLSJobRig{db: db, store: store, h: h, router: router}
}

// addAccount inserts a Codex OAuth account, loads it into the runtime pool and binds it to the given proxy.
func (r *codexRestoredTLSJobRig) addAccount(t *testing.T, name, token, proxyURL string) int64 {
	t.Helper()
	id, err := r.db.InsertAccountWithCredentials(context.Background(), name, map[string]interface{}{"access_token": token, "models": []string{"gpt-5.5"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	r.store.AddAccount(&auth.Account{DBID: id, AccessToken: token, ProxyURL: proxyURL, Models: []string{"gpt-5.5"}, PlanType: "pro", Status: auth.StatusReady})
	return id
}

func (r *codexRestoredTLSJobRig) create(id int64, body string) (*httptest.ResponseRecorder, database.QualityTestJob) {
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/accounts/%d/quality-test", id), strings.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	r.router.ServeHTTP(response, req)
	// The submitting browser goes away; the detached job must keep running.
	cancel()
	var created struct {
		Job database.QualityTestJob `json:"job"`
	}
	_ = json.Unmarshal(response.Body.Bytes(), &created)
	return response, created.Job
}

func TestQualityTestJobsSurviveRequestCancellationAndLimitThreeAccounts_CodexUpstream(t *testing.T) {
	if codexRestoredTLSRunInChild(t) {
		return
	}
	release := make(chan struct{})
	started := make(chan struct{}, 8)
	proxyURL := codexRestoredTLSUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"<html><svg>\"}\n\n")
		w.(http.Flusher).Flush()
		started <- struct{}{}
		// The account that gets cancelled is never released: only its runner stopping the request ends it, so its stored output stays partial.
		if r.Header.Get("Authorization") == "Bearer cancel-key" {
			<-r.Context().Done()
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-release:
		}
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"</svg></html>\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	})
	rig := newCodexRestoredTLSJobRig(t)
	var ids []int64
	for i := 0; i < 4; i++ {
		token := "test-key"
		if i == 0 {
			token = "cancel-key"
		}
		ids = append(ids, rig.addAccount(t, fmt.Sprintf("account-%d", i), token, proxyURL))
	}
	const body = `{"model":"gpt-5.5","reasoning_effort":"high","prompt":"HTML"}`
	var jobs []database.QualityTestJob
	for _, id := range ids[:3] {
		response, job := rig.create(id, body)
		if response.Code != http.StatusAccepted {
			t.Fatalf("create=%d %s", response.Code, response.Body.String())
		}
		jobs = append(jobs, job)
	}
	for range 3 {
		select {
		case <-started:
		case <-time.After(10 * time.Second):
			t.Fatal("detached job did not reach upstream")
		}
	}
	for _, job := range jobs {
		stored, err := rig.db.GetQualityTestJob(context.Background(), job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Status != "running" {
			t.Fatalf("request cancellation stopped job: %+v", stored)
		}
	}
	if response, _ := rig.create(ids[3], body); response.Code != http.StatusConflict {
		t.Fatalf("fourth task accepted: %d", response.Code)
	}
	if response, _ := rig.create(ids[0], body); response.Code != http.StatusConflict {
		t.Fatalf("duplicate account accepted: %d", response.Code)
	}
	response := httptest.NewRecorder()
	rig.router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, fmt.Sprintf("/quality-tests/%d/cancel", jobs[0].ID), nil))
	if response.Code != http.StatusOK {
		t.Fatal(response.Body.String())
	}
	// Cancelling only marks the job "cancelling"; its slot is freed when the runner notices on its own one-second ticker and finalises. There is no event for that moment, so freeing the slot while the other jobs still run cannot be asserted without polling or a production hook. This check therefore only proves that no slot stays occupied once every job has ended.
	close(release)
	rig.h.WaitQualityTests()
	if response, _ := rig.create(ids[3], body); response.Code != http.StatusAccepted {
		t.Fatalf("slot still occupied after every job ended: %d %s", response.Code, response.Body.String())
	}
	rig.h.WaitQualityTests()
	if stopped, err := rig.db.GetQualityTestJob(context.Background(), jobs[0].ID); err != nil || stopped.Status != "stopped" || stopped.Output != "<html><svg>" {
		t.Fatalf("cancelled job: %+v %v", stopped, err)
	}
	for _, job := range jobs[1:] {
		stored, err := rig.db.GetQualityTestJob(context.Background(), job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Status != "completed" || stored.Output != "<html><svg></svg></html>" || stored.ReasoningEffort != "high" || stored.AccountName == "" || stored.CompletedAt == nil {
			t.Fatalf("missing persisted result: %+v", stored)
		}
	}
}

func TestQualityTestJobRetriesOnlyBreaksBeforeOutput_CodexUpstream(t *testing.T) {
	if codexRestoredTLSRunInChild(t) {
		return
	}
	previousDelay := qualityTestRetryDelay
	qualityTestRetryDelay = 0
	defer func() { qualityTestRetryDelay = previousDelay }()
	cases := []struct {
		name        string
		attempt     func(n int, w http.ResponseWriter)
		wantStatus  string
		wantOutput  string
		wantRetries int
		wantBreak   bool
		wantCalls   int
	}{{
		name: "break before output retries",
		attempt: func(n int, w http.ResponseWriter) {
			if n == 1 {
				panic(http.ErrAbortHandler)
			}
			fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"<html></html>\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
		},
		wantStatus: "completed", wantOutput: "<html></html>", wantRetries: 1, wantCalls: 2,
	}, {
		name: "break after output keeps partial",
		attempt: func(_ int, w http.ResponseWriter) {
			fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"<html><svg>\"}\n\n")
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		},
		wantStatus: "error", wantOutput: "<html><svg>", wantBreak: true, wantCalls: 1,
	}, {
		name: "repeated break gives up after one retry",
		attempt: func(_ int, _ http.ResponseWriter) {
			panic(http.ErrAbortHandler)
		},
		wantStatus: "error", wantRetries: 1, wantBreak: true, wantCalls: 2,
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			proxyURL := codexRestoredTLSUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				n := int(calls.Add(1))
				w.Header().Set("Content-Type", "text/event-stream")
				tc.attempt(n, w)
			})
			rig := newCodexRestoredTLSJobRig(t)
			id := rig.addAccount(t, "account", "test-key", proxyURL)
			response, job := rig.create(id, `{"model":"gpt-5.5","prompt":"HTML"}`)
			if response.Code != http.StatusAccepted {
				t.Fatalf("create=%d %s", response.Code, response.Body.String())
			}
			rig.h.WaitQualityTests()
			stored, err := rig.db.GetQualityTestJob(context.Background(), job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Status != tc.wantStatus || stored.Output != tc.wantOutput || stored.Retries != tc.wantRetries || stored.Interrupted != tc.wantBreak || int(calls.Load()) != tc.wantCalls {
				t.Fatalf("got status=%s output=%q retries=%d interrupted=%v calls=%d error=%q", stored.Status, stored.Output, stored.Retries, stored.Interrupted, calls.Load(), stored.Error)
			}
		})
	}
}

func TestIntelligenceExecutorPinsPromptAndFinishesBeforeEOF_CodexUpstream(t *testing.T) {
	if codexRestoredTLSRunInChild(t) {
		return
	}
	const prompt = "题面\n最终只恢复数字结果。"
	h, a := codexRestoredTLSHandler(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if gjson.GetBytes(body, "input.0.content.0.text").String() != prompt || gjson.GetBytes(body, "instructions").String() != "" || gjson.GetBytes(body, "model").String() != "gpt-5.5" {
			t.Errorf("payload changed: %s", body)
		}
		if r.Header.Get("Authorization") != "Bearer sk-test-secret" || r.Header.Get("Cookie") != "" {
			t.Error("wrong identity")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.reasoning_text.delta\",\"delta\":\"wrong reasoning\"}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"29\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"model\":\"actual\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"21\"}]}],\"usage\":{\"input_tokens\":12,\"output_tokens\":8}}}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	h.store.AddAccount(codexRestoredTLSAccount(43, "http://127.0.0.1:1"))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result := h.executeIntelligenceCheck(ctx, a, "gpt-5.5", "high", prompt)
	if !result.Complete || result.Error != "" || result.Output != "21" || result.ResponseModel != "actual" || result.InputTokens != 12 {
		t.Fatalf("result=%+v", result)
	}
	// The handler blocks until the client goes away, so finishing at all proves the executor stops at the terminal event instead of waiting for EOF.
	if result.DurationMS > 15000 {
		t.Fatal("waited for EOF")
	}
}

func TestIntelligenceTimeoutKeepsPartialAnswerAsError_CodexUpstream(t *testing.T) {
	if codexRestoredTLSRunInChild(t) {
		return
	}
	h, a := codexRestoredTLSHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"21\"}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result := h.executeIntelligenceCheck(ctx, a, "gpt-5.5", "high", "answer")
	if result.Complete || result.Error == "" || result.Output != "21" {
		t.Fatalf("timeout result=%+v", result)
	}
}

func TestIntelligenceStreamErrorsOverridePartialAnswers_CodexUpstream(t *testing.T) {
	for _, wire := range []string{
		`data: {"type":"response.output_text.delta","delta":"21"}` + "\n\n",
		`data: {"type":"response.output_text.delta","delta":"21"}` + "\n\n" + `data: {"type":"response.incomplete","response":{"status":"incomplete"}}` + "\n\n",
		`data: {"type":"response.completed","response":{"status":"failed"}}` + "\n\n",
	} {
		result := readIntelligenceStream(strings.NewReader(wire))
		if result.Complete || result.Error == "" {
			t.Fatalf("must fail incomplete stream: %+v", result)
		}
	}
}

func TestIntelligenceSuccessDoesNotClearUnrelatedAccountState_CodexUpstream(t *testing.T) {
	if codexRestoredTLSRunInChild(t) {
		return
	}
	h, a := codexRestoredTLSHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"21\"}]}]}}\n\n")
	})
	a.Status = auth.StatusCooldown
	a.CooldownReason = "unauthorized"
	a.CooldownUtil = time.Now().Add(time.Hour)
	a.DispatchPaused = 1
	result := h.executeIntelligenceCheck(context.Background(), a, "gpt-5.5", "high", "answer")
	if !result.Complete || a.Status != auth.StatusCooldown || a.CooldownReason != "unauthorized" || a.DispatchPaused != 1 {
		t.Fatalf("unrelated state was reset: %+v", result)
	}
	if result.EffectiveContext["attempts"] != "1" {
		t.Fatalf("missing actual attempt audit: %+v", result.EffectiveContext)
	}
}

func TestIntelligenceAdminAPIAndRunner_CodexUpstream(t *testing.T) {
	if codexRestoredTLSRunInChild(t) {
		return
	}
	db := newTestAdminDB(t)
	id := insertTestAccount(t, db)
	var requests atomic.Int32
	proxyURL := codexRestoredTLSUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", codexRestoredTLSIntelligenceSuccess)
	})
	ctx := context.Background()
	if err := db.UpdateCredentials(ctx, id, map[string]interface{}{"access_token": "fixture-key", "models": []string{"gpt-5.4"}}); err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(db, nil, nil)
	if err := store.LoadAccountByID(ctx, id); err != nil {
		t.Fatal(err)
	}
	account := store.FindByID(id)
	if account == nil {
		t.Fatalf("expected a Codex account in the pool: %+v", account)
	}
	account.Mu().Lock()
	account.ProxyURL = proxyURL
	account.Mu().Unlock()
	h := &Handler{store: store, db: db, adminSecretEnv: "local-admin-key"}
	router := gin.New()
	group := router.Group("/api/admin")
	group.Use(h.adminAuthMiddleware())
	h.registerIntelligenceCheckRoutes(group)
	request := func(method, path, body, key string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/api/admin/intelligence-check/"+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	if w := request("GET", "settings", "", "wrong"); w.Code != 401 {
		t.Fatalf("auth=%d %s", w.Code, w.Body.String())
	}
	settings, _ := db.GetIntelligenceCheckSettings(ctx)
	settings.GPTModel = "gpt-5.4"
	raw, _ := json.Marshal(settings)
	if w := request("PUT", "settings", string(raw), "local-admin-key"); w.Code != 200 {
		t.Fatalf("save=%d %s", w.Code, w.Body.String())
	}
	if w := request("PUT", "settings", string(raw), "local-admin-key"); w.Code != 409 {
		t.Fatalf("CAS=%d %s", w.Code, w.Body.String())
	}
	var created struct {
		Runs []database.IntelligenceCheckRun `json:"runs"`
	}
	body := fmt.Sprintf(`{"account_ids":[%d]}`, id)
	for i := 0; i < 2; i++ {
		w := request("POST", "runs", body, "local-admin-key")
		if w.Code != 202 {
			t.Fatalf("create=%d %s", w.Code, w.Body.String())
		}
		json.Unmarshal(w.Body.Bytes(), &created)
	}
	if len(created.Runs) != 1 {
		t.Fatal(created)
	}
	// One scheduler tick claims the queued run and starts it; the wait group returns when the run has been finished and stored.
	if err := h.intelligenceTick(ctx); err != nil {
		t.Fatal(err)
	}
	h.WaitIntelligenceChecks()
	run, _ := db.GetIntelligenceCheckRun(ctx, created.Runs[0].ID)
	if run.Status != "normal" || requests.Load() != 1 {
		t.Fatalf("run=%+v requests=%d", run, requests.Load())
	}
	w := request("GET", fmt.Sprintf("bars?account_ids=%d", id), "", "local-admin-key")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"normal"`) {
		t.Fatalf("bars=%d %s", w.Code, w.Body.String())
	}
}

func TestIntelligenceRuntimeConfigurationFailureIsInterrupted_CodexUpstream(t *testing.T) {
	db := newTestAdminDB(t)
	id := insertTestAccount(t, db)
	ctx := context.Background()
	// The account is bound to a dead proxy and has no model for the configured check, so the run fails locally before any request is made.
	if err := db.UpdateCredentials(ctx, id, map[string]interface{}{"access_token": "fixture-key", "proxy_url": "http://127.0.0.1:1", "models": []string{"gpt-5.4"}}); err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(db, nil, nil)
	if err := store.LoadAccountByID(ctx, id); err != nil {
		t.Fatal(err)
	}
	h := &Handler{db: db, store: store}
	ids, err := db.EnqueueIntelligenceChecks(ctx, []int64{id}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	run, err := db.ClaimIntelligenceCheck(ctx, "test", time.Now(), 30*time.Second)
	if err != nil || run == nil {
		t.Fatalf("claim=%v %v", run, err)
	}
	h.runIntelligenceCheck(ctx, *run, store.FindByID(id))
	got, err := db.GetIntelligenceCheckRun(ctx, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "interrupted" || got.Error == "" {
		t.Fatalf("local failure misclassified: %+v", got)
	}
}
