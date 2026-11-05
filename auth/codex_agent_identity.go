package auth

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/nacl/box"
)

// Codex Agent Identity (auth_mode=agentIdentity) authorization:
// The account does not store OAuth access/refresh tokens. Instead it holds an Ed25519 private key (PKCS#8 base64) and
// agent_runtime_id. For each upstream request, the private key signs "runtime_id:task_id:timestamp" and puts the result in
// the Authorization: AgentAssertion <base64url(envelope)> header. The task_id is first obtained from the task registration endpoint
// (the response may be encrypted; it is decrypted with a key derived from the private key seed via X25519/NaCl box), then cached and persisted.
//
// The logic follows the agent identity implementation in sub2api, adapted to this project's Account/Store architecture.

const CodexAuthModeAgentIdentity = "agentIdentity"

const (
	agentIdentityAuthAPIBaseURL          = "https://auth.openai.com/api/accounts"
	agentIdentityTaskRegistrationTimeout = 30 * time.Second
)

// agentIdentityAuthAPIBase can be overridden in tests.
var agentIdentityAuthAPIBase = agentIdentityAuthAPIBaseURL

// agentIdentityTaskLocks serializes task registration per account, to avoid concurrent duplicate registrations.
var agentIdentityTaskLocks sync.Map // map[int64]*sync.Mutex

// IsCodexAgentIdentity reports whether the account is an Agent Identity authorized account.
func (a *Account) IsCodexAgentIdentity() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.isCodexAgentIdentityLocked()
}

func (a *Account) isCodexAgentIdentityLocked() bool {
	return strings.EqualFold(strings.TrimSpace(a.CodexAuthMode), CodexAuthModeAgentIdentity) &&
		strings.TrimSpace(a.AgentRuntimeID) != "" &&
		strings.TrimSpace(a.AgentPrivateKey) != ""
}

// parseAgentIdentityPrivateKey parses an Ed25519 private key in PKCS#8 base64 (the key contents are not logged).
func parseAgentIdentityPrivateKey(raw string) (ed25519.PrivateKey, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("agent identity private key is missing")
	}
	der, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, errors.New("agent identity private key is not valid base64")
	}
	key, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, errors.New("agent identity private key is not valid PKCS#8")
	}
	privateKey, ok := key.(ed25519.PrivateKey)
	if !ok || len(privateKey) != ed25519.PrivateKeySize {
		return nil, errors.New("agent identity private key is not Ed25519")
	}
	return privateKey, nil
}

// ValidateCodexAgentIdentityPrivateKey only checks the key format and does not return or log the key.
func ValidateCodexAgentIdentityPrivateKey(encoded string) error {
	_, err := parseAgentIdentityPrivateKey(encoded)
	return err
}

// agentIdentityRuntimeKey is the runtime credentials snapshot needed to build a signature.
type agentIdentityRuntimeKey struct {
	runtimeID  string
	taskID     string
	privateKey ed25519.PrivateKey
}

func (a *Account) agentIdentityRuntimeKeyLocked() (agentIdentityRuntimeKey, error) {
	privateKey, err := parseAgentIdentityPrivateKey(a.AgentPrivateKey)
	if err != nil {
		return agentIdentityRuntimeKey{}, err
	}
	runtimeID := strings.TrimSpace(a.AgentRuntimeID)
	if runtimeID == "" {
		return agentIdentityRuntimeKey{}, errors.New("agent identity runtime id is missing")
	}
	return agentIdentityRuntimeKey{
		runtimeID:  runtimeID,
		taskID:     strings.TrimSpace(a.AgentTaskID),
		privateKey: privateKey,
	}, nil
}

// BuildCodexAgentAssertion generates the Authorization value for one request (AgentAssertion <...>).
// Signing is purely local and makes no network request; it requires the task_id to be ready (guaranteed by EnsureCodexAgentIdentityTask).
func (a *Account) BuildCodexAgentAssertion(now time.Time) (string, error) {
	if a == nil {
		return "", errors.New("account is nil")
	}
	a.mu.RLock()
	key, err := a.agentIdentityRuntimeKeyLocked()
	a.mu.RUnlock()
	if err != nil {
		return "", err
	}
	return buildAgentAssertion(key, now)
}

func buildAgentAssertion(key agentIdentityRuntimeKey, now time.Time) (string, error) {
	if key.runtimeID == "" || key.taskID == "" {
		return "", errors.New("agent identity runtime or task id is missing")
	}
	timestamp := now.UTC().Format(time.RFC3339)
	payload := []byte(key.runtimeID + ":" + key.taskID + ":" + timestamp)
	signature, err := key.privateKey.Sign(nil, payload, crypto.Hash(0))
	if err != nil {
		return "", errors.New("failed to sign agent assertion")
	}
	envelope := map[string]string{
		"agent_runtime_id": key.runtimeID,
		"task_id":          key.taskID,
		"timestamp":        timestamp,
		"signature":        base64.StdEncoding.EncodeToString(signature),
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return "", errors.New("failed to serialize agent assertion")
	}
	return "AgentAssertion " + base64.RawURLEncoding.EncodeToString(encoded), nil
}

func signAgentTaskRegistration(key agentIdentityRuntimeKey, timestamp time.Time) (string, string, error) {
	if key.runtimeID == "" {
		return "", "", errors.New("agent identity runtime id is missing")
	}
	formatted := timestamp.UTC().Format(time.RFC3339)
	signature, err := key.privateKey.Sign(nil, []byte(key.runtimeID+":"+formatted), crypto.Hash(0))
	if err != nil {
		return "", "", errors.New("failed to sign agent task registration")
	}
	return formatted, base64.StdEncoding.EncodeToString(signature), nil
}

// decryptAgentTaskID decrypts the encrypted task_id returned by the registration endpoint (NaCl anonymous box,
// the recipient's public and private keys are derived from the Ed25519 seed via SHA-512 into Curve25519 keys).
func decryptAgentTaskID(key agentIdentityRuntimeKey, encoded string) (string, error) {
	ciphertext, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return "", errors.New("encrypted task id is not valid base64")
	}
	seed := key.privateKey.Seed()
	digest := sha512.Sum512(seed)
	var curvePrivate [32]byte
	copy(curvePrivate[:], digest[:32])
	curvePrivate[0] &= 248
	curvePrivate[31] &= 127
	curvePrivate[31] |= 64
	curvePublicBytes, err := curve25519.X25519(curvePrivate[:], curve25519.Basepoint)
	if err != nil {
		return "", errors.New("failed to derive the agent identity decryption public key")
	}
	var curvePublic [32]byte
	copy(curvePublic[:], curvePublicBytes)
	plaintext, ok := box.OpenAnonymous(nil, ciphertext, &curvePublic, &curvePrivate)
	if !ok {
		return "", errors.New("failed to decrypt the encrypted task id")
	}
	taskID := strings.TrimSpace(string(plaintext))
	if taskID == "" {
		return "", errors.New("decrypted task id is empty")
	}
	return taskID, nil
}

type agentIdentityTaskRegistrationResponse struct {
	TaskID               string `json:"task_id"`
	TaskIDCamel          string `json:"taskId"`
	EncryptedTaskID      string `json:"encrypted_task_id"`
	EncryptedTaskIDCamel string `json:"encryptedTaskId"`
}

// registerAgentIdentityTask registers a new task with OpenAI and returns the task_id.
func registerAgentIdentityTask(ctx context.Context, key agentIdentityRuntimeKey, proxyURL string) (string, error) {
	timestamp, signature, err := signAgentTaskRegistration(key, time.Now())
	if err != nil {
		return "", err
	}
	boundProxy, accountBound := AccountBoundEgress(ctx)
	if accountBound {
		proxyURL = boundProxy
	}
	client, err := proxiedHTTPClient(proxyURL)
	if err != nil {
		return "", err
	}
	if accountBound {
		if proxyURL == "" {
			client.Transport.(*http.Transport).Proxy = nil
		}
		defer client.CloseIdleConnections()
	}
	client.Timeout = agentIdentityTaskRegistrationTimeout
	body, err := json.Marshal(map[string]string{"timestamp": timestamp, "signature": signature})
	if err != nil {
		return "", errors.New("failed to serialize agent task registration request")
	}
	url := strings.TrimRight(strings.TrimSpace(agentIdentityAuthAPIBase), "/") + "/v1/agent/" + key.runtimeID + "/task/register"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return "", errors.New("failed to build agent task registration request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", errors.New("agent task registration request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("agent task registration returned status %d", resp.StatusCode)
	}
	var result agentIdentityTaskRegistrationResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&result); err != nil {
		return "", errors.New("agent task registration response is invalid")
	}
	if taskID := strings.TrimSpace(result.TaskID); taskID != "" {
		return taskID, nil
	}
	if taskID := strings.TrimSpace(result.TaskIDCamel); taskID != "" {
		return taskID, nil
	}
	encrypted := strings.TrimSpace(result.EncryptedTaskID)
	if encrypted == "" {
		encrypted = strings.TrimSpace(result.EncryptedTaskIDCamel)
	}
	if encrypted == "" {
		return "", errors.New("agent task registration response did not include a task id")
	}
	return decryptAgentTaskID(key, encrypted)
}

// EnsureCodexAgentIdentityTask ensures the account holds a usable task_id: it registers one when missing (or when a forced refresh is needed),
// then persists it (credentials.task_id) on success and caches it on the runtime account. Serialized per account to avoid concurrent duplicate registrations.
// With forceRefresh=true, the existing task_id is ignored and a new one is registered (used to recover from a 401 when the task has expired).
func (s *Store) EnsureCodexAgentIdentityTask(ctx context.Context, account *Account, forceRefresh bool) error {
	if account == nil || !account.IsCodexAgentIdentity() {
		return nil
	}
	if !forceRefresh {
		account.mu.RLock()
		hasTask := strings.TrimSpace(account.AgentTaskID) != ""
		account.mu.RUnlock()
		if hasTask {
			return nil
		}
	}

	lockKey := account.ID()
	candidate := &sync.Mutex{}
	actual, _ := agentIdentityTaskLocks.LoadOrStore(lockKey, candidate)
	mu, _ := actual.(*sync.Mutex)
	if mu == nil {
		mu = candidate
	}
	mu.Lock()
	defer mu.Unlock()

	// After taking the lock, check again: another request may have completed the registration already.
	account.mu.RLock()
	key, keyErr := account.agentIdentityRuntimeKeyLocked()
	existingTask := strings.TrimSpace(account.AgentTaskID)
	proxyURL := account.ProxyURL
	account.mu.RUnlock()
	if keyErr != nil {
		return keyErr
	}
	if !forceRefresh && existingTask != "" {
		return nil
	}

	taskID, err := registerAgentIdentityTask(ctx, key, proxyURL)
	if err != nil {
		return err
	}

	account.mu.Lock()
	account.AgentTaskID = taskID
	account.mu.Unlock()

	if s != nil && s.db != nil && account.DBID > 0 {
		if err := s.db.UpdateCredentials(ctx, account.DBID, map[string]interface{}{"task_id": taskID}); err != nil {
			return fmt.Errorf("failed to persist agent task id: %w", err)
		}
	}
	return nil
}

// IsAgentIdentityTaskInvalidResponse reports whether an upstream 401 means the task is invalid (recoverable by registering again).
func IsAgentIdentityTaskInvalidResponse(statusCode int, body []byte) bool {
	if statusCode != http.StatusUnauthorized {
		return false
	}
	lower := strings.ToLower(string(body))
	compact := strings.NewReplacer(" ", "", "\t", "", "\r", "", "\n", "").Replace(lower)
	for _, marker := range []string{
		`"code":"invalid_task_id"`,
		`"code":"task_not_found"`,
		`"code":"task_expired"`,
		`"error":"invalid_task_id"`,
	} {
		if strings.Contains(compact, marker) {
			return true
		}
	}
	for _, marker := range []string{
		"invalid task_id", "invalid task id", "task_id is invalid",
		"task id is invalid", "task not found", "task expired",
		"unknown task_id", "unknown task id",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}
