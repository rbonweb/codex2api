package proxy

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Continuity holds session continuity information
type Continuity struct {
	Key    string // session key
	Source string // source identifier
}

// Request is the request structure (used for continuity resolution)
type Request struct {
	Payload []byte      // request body
	Headers http.Header // request headers
}

// Options is the options structure (used to pass metadata)
type Options struct {
	Metadata map[string]any // metadata
}

// Metadata constant definitions
const (
	ExecutionSessionMetadataKey = "execution_session"
	SelectedAuthMetadataKey     = "selected_auth"
)

// metadataString extracts a string value from the metadata
func metadataString(meta map[string]any, key string) string {
	if len(meta) == 0 {
		return ""
	}
	raw, ok := meta[key]
	if !ok || raw == nil {
		return ""
	}
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case []byte:
		return strings.TrimSpace(string(v))
	default:
		return ""
	}
}

// principalString converts an arbitrary value to a string
func principalString(raw any) string {
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case fmt.Stringer:
		return strings.TrimSpace(v.String())
	default:
		return strings.TrimSpace(fmt.Sprintf("%v", raw))
	}
}

// getGinContext tries to get the gin.Context from the context
func getGinContext(ctx context.Context) *gin.Context {
	if ctx == nil {
		return nil
	}
	// gin.Context is usually stored under the key "gin-context"
	if c, ok := ctx.Value("gin-context").(*gin.Context); ok {
		return c
	}
	// *gin.Context itself does not implement context.Context, so it cannot be type-asserted directly
	// it must be fetched via the key stored in the context
	return nil
}

// ResolveContinuity resolves session continuity
// Priority (highest to lowest):
// 1. prompt_cache_key (request body)
// 2. execution_session (metadata)
// 3. idempotency-key (request header)
// 4. client_principal (apiKey in the gin context)
// 5. auth_id (account ID)
//
// Deprecated: the live continuity/session identity keys are implemented in executor.go (ResolveSessionID /
// resolveUpstreamSessionID / ExecuteRequest). This function no longer has callers on the production path (only test references),
// do not wire it back in: its levels 4/5 would collapse requests without an explicit session onto one upstream identity by API key/account,
// which is the root cause of the issue #268 context pollution. Make changes to the new logic in executor.go.
func ResolveContinuity(ctx context.Context, account *auth.Account, req Request, opts Options) Continuity {
	// 1. Highest priority: prompt_cache_key in the request body
	if promptCacheKey := strings.TrimSpace(gjson.GetBytes(req.Payload, "prompt_cache_key").String()); promptCacheKey != "" {
		return Continuity{Key: promptCacheKey, Source: "prompt_cache_key"}
	}

	// 2. execution_session in the metadata
	if executionSession := metadataString(opts.Metadata, ExecutionSessionMetadataKey); executionSession != "" {
		return Continuity{Key: executionSession, Source: "execution_session"}
	}

	// 3. Idempotency-Key in the request headers
	if req.Headers != nil {
		if v := strings.TrimSpace(req.Headers.Get("Idempotency-Key")); v != "" {
			return Continuity{Key: v, Source: "idempotency_key"}
		}
	}

	// 4. apiKey in the gin context (client_principal)
	// read from the Authorization header first
	apiKey := ""
	if ginCtx := getGinContext(ctx); ginCtx != nil {
		// try to read from the Authorization header
		authHeader := ginCtx.GetHeader("Authorization")
		if authHeader != "" {
			apiKey = strings.TrimPrefix(authHeader, "Bearer ")
			apiKey = strings.TrimSpace(apiKey)
		}
		// fallback: try to get it from the gin context (if middleware set it)
		if apiKey == "" {
			if v, exists := ginCtx.Get("apiKey"); exists && v != nil {
				apiKey = principalString(v)
			}
		}
	}
	if apiKey != "" {
		return Continuity{
			Key:    uuid.NewSHA1(uuid.NameSpaceOID, []byte("codex2api:prompt-cache:"+apiKey)).String(),
			Source: "client_principal",
		}
	}

	// 5. Lowest priority: generate a deterministic UUID from auth.ID
	if account != nil {
		account.Mu().RLock()
		authID := strings.TrimSpace(account.Email)
		if authID == "" {
			// only use the ID as the fallback when DBID > 0
			if account.DBID > 0 {
				authID = fmt.Sprintf("%d", account.DBID)
			}
		}
		account.Mu().RUnlock()

		if authID != "" {
			return Continuity{
				Key:    uuid.NewSHA1(uuid.NameSpaceOID, []byte("codex2api:prompt-cache:auth:"+authID)).String(),
				Source: "auth_id",
			}
		}
	}

	return Continuity{}
}

// ApplyContinuityBody applies session continuity to the request body
// injects continuity.Key into the prompt_cache_key field
func ApplyContinuityBody(body []byte, c Continuity) []byte {
	if c.Key == "" {
		return body
	}
	body, _ = sjson.SetBytes(body, "prompt_cache_key", c.Key)
	return body
}

// ApplyContinuityHeaders applies session continuity to the request headers
// sets the session_id header to keep session continuity
func ApplyContinuityHeaders(headers http.Header, c Continuity) {
	if headers == nil || c.Key == "" {
		return
	}
	headers.Set("session_id", c.Key)
}

// GetSessionID gets the session_id from the continuity information (backward compatible)
func (c Continuity) GetSessionID() string {
	return c.Key
}

// IsEmpty checks whether the continuity is empty
func (c Continuity) IsEmpty() bool {
	return c.Key == ""
}
