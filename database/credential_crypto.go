package database

// Account credentials encryption at rest (optional, off by default).
//
// Design goal: encrypt the sensitive fields in the credentials JSONB (access_token / refresh_token /
// session_token / api_key / id_token / agent_private_key / client_secret) on write,
// and decrypt them on read, without **changing any upper-layer caller**, and without breaking the platform's two existing kinds of SQL dependency:
//   1. the scheduling outbox trigger does **change detection** on access_token and the others via OLD/NEW;
//   2. the account list projection does an **existence check** with `<> ''`.
// To that end it uses a **deterministic AEAD** (the nonce is derived from HMAC(key, field||plaintext)): the same plaintext always
// yields the same ciphertext, so change detection semantics are unchanged; a non-empty ciphertext keeps the existence check unchanged.
//
// Switch: environment variable CODEX_CRED_ENCRYPTION_KEY. When unset, all functions are no-ops and behavior is exactly the same as without encryption
// (existing plaintext accounts keep working). When set: newly written sensitive fields are encrypted, and the read side decrypts them transparently;
// existing plaintext rows, which have no enc: prefix, are returned as-is and keep working (gradual migration, converted to ciphertext automatically on rewrite).
//
// Note: if the key is lost, encrypted credentials cannot be decrypted and the related accounts must be re-imported; this is the inherent cost of encryption.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"os"
	"strings"
	"sync"
)

const credEncPrefix = "enc:v1:"

// sensitiveCredentialKeys are the credential fields to encrypt. Only these fields are encrypted; upstream_type /
// email / plan_type / models and other fields used in SQL filtering stay in plaintext.
var sensitiveCredentialKeys = map[string]struct{}{
	"access_token":      {},
	"refresh_token":     {},
	"session_token":     {},
	"api_key":           {},
	"id_token":          {},
	"agent_private_key": {},
	"client_secret":     {},
}

var (
	credKeyOnce sync.Once
	credKey     []byte // 32 bytes; nil means not enabled
)

// credCipherKey reads and derives the key lazily (SHA-256(env value) → 32 bytes). Returns nil when unset.
func credCipherKey() []byte {
	credKeyOnce.Do(func() {
		if v := strings.TrimSpace(os.Getenv("CODEX_CRED_ENCRYPTION_KEY")); v != "" {
			sum := sha256.Sum256([]byte(v))
			credKey = sum[:]
		}
	})
	return credKey
}

// setCredEncryptionKeyForTest injects or clears the key, for tests only.
func setCredEncryptionKeyForTest(raw string) {
	credKeyOnce.Do(func() {}) // mark the once as fired so a later env value does not override it
	if strings.TrimSpace(raw) == "" {
		credKey = nil
		return
	}
	sum := sha256.Sum256([]byte(raw))
	credKey = sum[:]
}

// encryptCredentialValue encrypts a single field value. Already encrypted, empty, or disabled values are returned as-is.
func encryptCredentialValue(field, plaintext string) string {
	key := credCipherKey()
	if key == nil || plaintext == "" || strings.HasPrefix(plaintext, credEncPrefix) {
		return plaintext
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return plaintext
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return plaintext
	}
	// Deterministic nonce: HMAC(key, field || 0x00 || plaintext) truncated to the nonce length.
	// The same plaintext always gets the same nonce and ciphertext (keeps change detection); different plaintexts almost always get different nonces (GCM safety).
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(field))
	mac.Write([]byte{0})
	mac.Write([]byte(plaintext))
	nonce := mac.Sum(nil)[:gcm.NonceSize()]
	// AAD=field binds the ciphertext to the field, preventing cross-field copying.
	ct := gcm.Seal(nil, nonce, []byte(plaintext), []byte(field))
	buf := make([]byte, 0, len(nonce)+len(ct))
	buf = append(buf, nonce...)
	buf = append(buf, ct...)
	return credEncPrefix + base64.RawURLEncoding.EncodeToString(buf)
}

// decryptCredentialValue decrypts a single field value. Values without the prefix, when disabled, or on decryption failure are returned as-is.
func decryptCredentialValue(field, value string) string {
	if !strings.HasPrefix(value, credEncPrefix) {
		return value
	}
	key := credCipherKey()
	if key == nil {
		return value
	}
	raw, err := base64.RawURLEncoding.DecodeString(value[len(credEncPrefix):])
	if err != nil {
		return value
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return value
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return value
	}
	if len(raw) < gcm.NonceSize() {
		return value
	}
	nonce, ct := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	pt, err := gcm.Open(nil, nonce, ct, []byte(field))
	if err != nil {
		return value
	}
	return string(pt)
}

// encryptSensitiveCredentials returns a shallow copy in which the sensitive fields are encrypted. When disabled, the input is returned as-is.
// It is called before marshaling in every write function.
func encryptSensitiveCredentials(m map[string]interface{}) map[string]interface{} {
	if credCipherKey() == nil || m == nil {
		return m
	}
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		if _, ok := sensitiveCredentialKeys[k]; ok {
			if s, isStr := v.(string); isStr {
				out[k] = encryptCredentialValue(k, s)
				continue
			}
		}
		out[k] = v
	}
	return out
}

// decryptSensitiveCredentialsInPlace decrypts the sensitive fields of the map in place. It is called in decodeCredentials
// so that all Go readers (GetCredential and direct map reads elsewhere) see plaintext uniformly.
func decryptSensitiveCredentialsInPlace(m map[string]interface{}) {
	if credCipherKey() == nil || m == nil {
		return
	}
	for k, v := range m {
		if _, ok := sensitiveCredentialKeys[k]; !ok {
			continue
		}
		if s, isStr := v.(string); isStr {
			m[k] = decryptCredentialValue(k, s)
		}
	}
}
