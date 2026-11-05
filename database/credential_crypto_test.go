package database

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestCredentialCrypto_RoundTrip(t *testing.T) {
	setCredEncryptionKeyForTest("test-master-key-123")
	defer setCredEncryptionKeyForTest("")

	m := map[string]interface{}{
		"upstream_type": "claude",
		"access_token":  "sk-at-secret",
		"refresh_token": "rt-secret",
		"email":         "user@example.com",
		"plan_type":     "claude",
	}
	enc := encryptSensitiveCredentials(m)
	// Sensitive fields should be encrypted (with the prefix); non-sensitive fields stay as they are.
	if !strings.HasPrefix(enc["access_token"].(string), credEncPrefix) {
		t.Fatalf("access_token is not encrypted: %v", enc["access_token"])
	}
	if !strings.HasPrefix(enc["refresh_token"].(string), credEncPrefix) {
		t.Fatalf("refresh_token is not encrypted")
	}
	if enc["email"] != "user@example.com" || enc["upstream_type"] != "claude" {
		t.Fatal("non-sensitive fields must not change")
	}
	// The original map must not be mutated (a copy is returned).
	if strings.HasPrefix(m["access_token"].(string), credEncPrefix) {
		t.Fatal("encryptSensitiveCredentials must not mutate its input")
	}

	// Simulate write then read: marshal(enc) followed by decodeCredentials should restore the plaintext.
	raw, _ := json.Marshal(enc)
	decoded := decodeCredentials(raw)
	if decoded["access_token"] != "sk-at-secret" || decoded["refresh_token"] != "rt-secret" {
		t.Fatalf("decryption failed to restore: at=%v rt=%v", decoded["access_token"], decoded["refresh_token"])
	}
}

func TestCredentialCrypto_Deterministic(t *testing.T) {
	setCredEncryptionKeyForTest("k")
	defer setCredEncryptionKeyForTest("")
	// Encrypting the same plaintext twice should give the same ciphertext (keeps the outbox change detection semantics).
	a := encryptCredentialValue("access_token", "same-token")
	b := encryptCredentialValue("access_token", "same-token")
	if a != b {
		t.Fatalf("deterministic encryption should produce the same ciphertext: %s vs %s", a, b)
	}
	// Different plaintexts should give different ciphertexts.
	c := encryptCredentialValue("access_token", "other-token")
	if a == c {
		t.Fatal("different plaintexts must not produce the same ciphertext")
	}
	// The same plaintext under different fields (AAD) should give different ciphertexts.
	d := encryptCredentialValue("refresh_token", "same-token")
	if a == d {
		t.Fatal("different fields must bind to different ciphertexts")
	}
}

func TestCredentialCrypto_Disabled_NoOp(t *testing.T) {
	setCredEncryptionKeyForTest("") // disabled
	defer setCredEncryptionKeyForTest("")
	m := map[string]interface{}{"access_token": "plain", "refresh_token": "plain2"}
	enc := encryptSensitiveCredentials(m)
	if enc["access_token"] != "plain" {
		t.Fatal("when disabled it should return the input as-is (no-op)")
	}
	raw, _ := json.Marshal(enc)
	decoded := decodeCredentials(raw)
	if decoded["access_token"] != "plain" {
		t.Fatal("when disabled, decryption should return the input as-is")
	}
}

func TestCredentialCrypto_BackwardCompat_PlaintextRows(t *testing.T) {
	// Existing plaintext rows: even with a key enabled, values without the enc: prefix should be read back as-is (gradual migration).
	setCredEncryptionKeyForTest("k")
	defer setCredEncryptionKeyForTest("")
	raw := []byte(`{"access_token":"legacy-plain","refresh_token":"legacy-rt","upstream_type":"codex"}`)
	decoded := decodeCredentials(raw)
	if decoded["access_token"] != "legacy-plain" || decoded["refresh_token"] != "legacy-rt" {
		t.Fatalf("existing plaintext should be read back as-is: %v", decoded)
	}
}

func TestCredentialCrypto_WrongKey_FailsClosed(t *testing.T) {
	setCredEncryptionKeyForTest("key-A")
	enc := encryptCredentialValue("access_token", "secret")
	// After switching keys, decryption fails and returns the original ciphertext (not plaintext); the account must be re-imported, so it is not mistakenly used as plaintext.
	setCredEncryptionKeyForTest("key-B")
	defer setCredEncryptionKeyForTest("")
	got := decryptCredentialValue("access_token", enc)
	if got == "secret" {
		t.Fatal("a wrong key must not decrypt to plaintext")
	}
	if !strings.HasPrefix(got, credEncPrefix) {
		t.Fatal("a failed decryption should return the original ciphertext")
	}
}

func TestCredentialCrypto_DBRoundTrip_AtRestEncrypted(t *testing.T) {
	setCredEncryptionKeyForTest("db-master-key")
	defer setCredEncryptionKeyForTest("")

	db, err := New("sqlite", filepath.Join(t.TempDir(), "cred-crypto.db"))
	if err != nil {
		t.Fatalf("New sqlite: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	id, err := db.InsertAccountWithUpstream(ctx, "claude", "anthropic", "oauth", map[string]interface{}{
		"upstream_type": "claude",
		"access_token":  "at-plain-secret",
		"refresh_token": "rt-plain-secret",
		"email":         "u@example.com",
	}, "")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Read back: GetCredential should see plaintext.
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if row.GetCredential("access_token") != "at-plain-secret" || row.GetCredential("refresh_token") != "rt-plain-secret" {
		t.Fatalf("read-back should be plaintext: at=%q rt=%q", row.GetCredential("access_token"), row.GetCredential("refresh_token"))
	}

	// The underlying storage should be ciphertext (enc: prefix).
	var rawCred string
	if err := db.conn.QueryRowContext(ctx, "SELECT credentials FROM accounts WHERE id = ?", id).Scan(&rawCred); err != nil {
		t.Fatalf("raw select: %v", err)
	}
	if strings.Contains(rawCred, "at-plain-secret") || strings.Contains(rawCred, "rt-plain-secret") {
		t.Fatalf("underlying storage must not contain plaintext tokens: %s", rawCred)
	}
	if !strings.Contains(rawCred, credEncPrefix) {
		t.Fatalf("underlying storage should be ciphertext (containing the %s prefix): %s", credEncPrefix, rawCred)
	}
	// email (non-sensitive) should still be plaintext, for SQL filtering.
	if !strings.Contains(rawCred, "u@example.com") {
		t.Fatalf("non-sensitive fields should stay plaintext: %s", rawCred)
	}

	// UpdateCredentials round trip: after refreshing the token, reading back is still plaintext.
	if err := db.UpdateCredentials(ctx, id, map[string]interface{}{"access_token": "at-refreshed"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	row2, _ := db.GetAccountByID(ctx, id)
	if row2.GetCredential("access_token") != "at-refreshed" {
		t.Fatalf("after update, read-back should be the new plaintext, got %q", row2.GetCredential("access_token"))
	}
}
