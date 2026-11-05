package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/nacl/box"
)

func newTestAgentKey(t *testing.T) (ed25519.PublicKey, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate Ed25519 key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("PKCS8 encoding failed: %v", err)
	}
	return pub, base64.StdEncoding.EncodeToString(der)
}

func TestValidateCodexAgentIdentityPrivateKey(t *testing.T) {
	_, pkcs8 := newTestAgentKey(t)
	if err := ValidateCodexAgentIdentityPrivateKey(pkcs8); err != nil {
		t.Fatalf("a valid PKCS8 Ed25519 private key should pass: %v", err)
	}
	if err := ValidateCodexAgentIdentityPrivateKey("not-base64!!"); err == nil {
		t.Fatal("invalid base64 should return an error")
	}
	if err := ValidateCodexAgentIdentityPrivateKey(base64.StdEncoding.EncodeToString([]byte("garbage"))); err == nil {
		t.Fatal("non-PKCS8 input should return an error")
	}
}

func TestBuildCodexAgentAssertionSignature(t *testing.T) {
	pub, pkcs8 := newTestAgentKey(t)
	acc := &Account{CodexAuthMode: CodexAuthModeAgentIdentity, AgentRuntimeID: "agent-RT123", AgentPrivateKey: pkcs8, AgentTaskID: "TASK-xyz"}
	if !acc.IsCodexAgentIdentity() {
		t.Fatal("should be recognized as an agent identity account")
	}
	assertion, err := acc.BuildCodexAgentAssertion(time.Now())
	if err != nil {
		t.Fatalf("failed to build assertion: %v", err)
	}
	if !strings.HasPrefix(assertion, "AgentAssertion ") {
		t.Fatalf("wrong assertion prefix: %q", assertion)
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(assertion, "AgentAssertion "))
	if err != nil {
		t.Fatalf("envelope is not base64url: %v", err)
	}
	var env map[string]string
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("envelope is not JSON: %v", err)
	}
	if env["agent_runtime_id"] != "agent-RT123" || env["task_id"] != "TASK-xyz" {
		t.Fatalf("wrong envelope fields: %+v", env)
	}
	payload := env["agent_runtime_id"] + ":" + env["task_id"] + ":" + env["timestamp"]
	sig, err := base64.StdEncoding.DecodeString(env["signature"])
	if err != nil {
		t.Fatalf("signature is not base64: %v", err)
	}
	if !ed25519.Verify(pub, []byte(payload), sig) {
		t.Fatal("assertion signature cannot be verified with the public key")
	}
}

func TestBuildAgentAssertionRequiresTaskID(t *testing.T) {
	_, pkcs8 := newTestAgentKey(t)
	acc := &Account{CodexAuthMode: CodexAuthModeAgentIdentity, AgentRuntimeID: "agent-RT123", AgentPrivateKey: pkcs8}
	if _, err := acc.BuildCodexAgentAssertion(time.Now()); err == nil {
		t.Fatal("missing task_id should return an error")
	}
}

// sealTaskIDForKey uses the Curve25519 public key derived from the private key to simulate the upstream SealAnonymous encryption of the task_id.
func sealTaskIDForKey(t *testing.T, pkcs8, taskID string) string {
	t.Helper()
	priv, err := parseAgentIdentityPrivateKey(pkcs8)
	if err != nil {
		t.Fatalf("failed to parse private key: %v", err)
	}
	seed := priv.Seed()
	digest := sha512.Sum512(seed)
	var curvePriv [32]byte
	copy(curvePriv[:], digest[:32])
	curvePriv[0] &= 248
	curvePriv[31] &= 127
	curvePriv[31] |= 64
	curvePubBytes, err := curve25519.X25519(curvePriv[:], curve25519.Basepoint)
	if err != nil {
		t.Fatalf("failed to derive public key: %v", err)
	}
	var curvePub [32]byte
	copy(curvePub[:], curvePubBytes)
	sealed, err := box.SealAnonymous(nil, []byte(taskID), &curvePub, rand.Reader)
	if err != nil {
		t.Fatalf("SealAnonymous failed: %v", err)
	}
	return base64.StdEncoding.EncodeToString(sealed)
}

func TestDecryptAgentTaskIDRoundTrip(t *testing.T) {
	_, pkcs8 := newTestAgentKey(t)
	priv, _ := parseAgentIdentityPrivateKey(pkcs8)
	key := agentIdentityRuntimeKey{runtimeID: "agent-RT123", privateKey: priv}
	encrypted := sealTaskIDForKey(t, pkcs8, "DECRYPTED-TASK-42")
	got, err := decryptAgentTaskID(key, encrypted)
	if err != nil {
		t.Fatalf("decryption failed: %v", err)
	}
	if got != "DECRYPTED-TASK-42" {
		t.Fatalf("wrong decryption result: %q", got)
	}
}

func TestRegisterAgentIdentityTaskDecryptsEncryptedResponse(t *testing.T) {
	_, pkcs8 := newTestAgentKey(t)
	encrypted := sealTaskIDForKey(t, pkcs8, "TASK-FROM-SERVER")

	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		if !strings.Contains(r.URL.Path, "/v1/agent/agent-RT123/task/register") {
			t.Errorf("wrong registration path: %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"encrypted_task_id": encrypted})
	}))
	defer srv.Close()

	prev := agentIdentityAuthAPIBase
	agentIdentityAuthAPIBase = srv.URL
	defer func() { agentIdentityAuthAPIBase = prev }()

	priv, _ := parseAgentIdentityPrivateKey(pkcs8)
	key := agentIdentityRuntimeKey{runtimeID: "agent-RT123", privateKey: priv}
	task, err := registerAgentIdentityTask(context.Background(), key, "")
	if err != nil {
		t.Fatalf("registration failed: %v", err)
	}
	if task != "TASK-FROM-SERVER" {
		t.Fatalf("wrong task id: %q", task)
	}
	// The registration request body should include timestamp + signature
	if gotBody["timestamp"] == "" || gotBody["signature"] == "" {
		t.Fatalf("registration request is missing timestamp/signature: %+v", gotBody)
	}
}

func TestIsAgentIdentityTaskInvalidResponse(t *testing.T) {
	if !IsAgentIdentityTaskInvalidResponse(401, []byte(`{"error":{"code":"invalid_task_id"}}`)) {
		t.Fatal("invalid_task_id should be treated as an invalid task")
	}
	if !IsAgentIdentityTaskInvalidResponse(401, []byte(`{"message":"task not found"}`)) {
		t.Fatal("task not found should be treated as an invalid task")
	}
	if IsAgentIdentityTaskInvalidResponse(500, []byte(`{"error":{"code":"invalid_task_id"}}`)) {
		t.Fatal("a non-401 response should not be treated as an invalid task")
	}
	if IsAgentIdentityTaskInvalidResponse(401, []byte(`{"error":"rate_limited"}`)) {
		t.Fatal("other 401 responses should not be treated as an invalid task")
	}
}
