package proxy

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

// x-codex-turn-state is an opaque turn state blob that the upstream mints in its response, and the client sends it back on the same
// turn's follow-up requests unchanged. The blob is bound to the outbound identity of the account that minted it, so replay on the same account is self-consistent;
// cross-account replay (after a failover to another account, the client still sends back the old account's blob) is a contradictory signal that only proxy chains produce and that
// real Codex never produces. The origin table records, for each downstream session, which account most recently issued this blob to the client;
// the outbound guard uses it to strip echoed values that belong to a known other account. It only strips and never injects:
// the downstream is a real Codex client, which sends the blob back on its own according to its turn semantics.
//
// The key uses affinityKey (downstream session ID + API Key, the same source as account stickiness binding), so that
// recording and guarding for the same conversation land on the same key. Without a session ID, no tracking is done (pass through unchanged).
const (
	codexTurnStateProvenanceTTL = time.Hour
	// The origin table is only used to strip cross-account echoes, so entries can be dropped safely; a hard cap prevents high cardinality from
	// session keys growing without bound within the TTL.
	codexTurnStateMaxOrigins = 65536
)

type codexTurnStateOrigin struct {
	accountID int64
	expiresAt time.Time
}

var (
	codexTurnStateOrigins sync.Map // affinityKey(string) -> codexTurnStateOrigin
	codexTurnStateWrites  atomic.Uint64
)

// relayCodexTurnStateResponseHeader writes the upstream response's turn-state into the downstream response header,
// and records the minting account. When the upstream does not send that header, it proactively clears any value left on the writer by the previous failover
// attempt — otherwise, after a retry on another account, the old account's blob would stick to the new account's response, which is exactly the
// cross-account contradiction this file exists to prevent. (Once a streaming response is committed, changes to the writer header are a harmless no-op.)
func relayCodexTurnStateResponseHeader(c *gin.Context, affinityKey string, account *auth.Account, model string, headers http.Header) {
	if c == nil {
		return
	}
	token := ""
	if headers != nil {
		token = strings.TrimSpace(headers.Get(codexTurnStateHeader))
	}
	if token == "" {
		c.Writer.Header().Del(codexTurnStateHeader)
		return
	}
	c.Header(codexTurnStateHeader, token)
	noteCodexTurnStateProvenance(affinityKey, account)
}

// commitResponsesStreamAttempt publishes the winning account's turn-state only
// when a retry heartbeat has not already committed the HTTP headers. The token
// must not be exposed before the attempt reaches a successful terminal because
// a later failover may use another account. If a heartbeat already committed
// the response, no documented Responses event is equivalent to this header, so
// the token is intentionally omitted instead of leaking stale account state.
func (h *Handler) commitResponsesStreamAttempt(c *gin.Context, attempt *continuousRetryStreamAttempt, affinityKey string, account *auth.Account, model string, headers http.Header) error {
	if attempt == nil {
		return h.commitStreamAttempt(c, attempt)
	}

	token := ""
	stagedHeader := false
	if headers != nil {
		token = strings.TrimSpace(headers.Get(codexTurnStateHeader))
	}
	if c != nil && c.Writer != nil && !c.Writer.Written() {
		stagedHeader = true
		if token == "" {
			c.Writer.Header().Del(codexTurnStateHeader)
		} else {
			c.Header(codexTurnStateHeader, token)
		}
	}

	if err := h.commitStreamAttempt(c, attempt); err != nil {
		// Headers are staged before replay/filter commit; remove the token on
		// failure so local replay errors cannot expose turn state or provenance.
		// The header is staged before replay/filtering commits it; on failure, remove the token so a local replay does not wrongly
		// expose account-bound continuation state or provenance data.
		if stagedHeader && c != nil && c.Writer != nil && !c.Writer.Written() {
			c.Writer.Header().Del(codexTurnStateHeader)
		}
		return err
	}
	if stagedHeader && token != "" {
		noteCodexTurnStateProvenance(affinityKey, account)
	}
	return nil
}

// guardCodexTurnStateEcho is the outbound guard: if the turn-state echoed back by the client is known to have been minted by another account
// it is stripped from the downstream header (both HTTP pass-through and the WS handshake read the value from this header). For the same account, or with no provenance
// record, it is left as is. It is called per attempt: after a failover to another account, the next attempt for the same request must
// re-decide.
func guardCodexTurnStateEcho(affinityKey string, account *auth.Account, headers http.Header) {
	if headers == nil || account == nil || strings.TrimSpace(affinityKey) == "" {
		return
	}
	if strings.TrimSpace(headers.Get(codexTurnStateHeader)) == "" {
		return
	}
	raw, ok := codexTurnStateOrigins.Load(affinityKey)
	if !ok {
		return
	}
	origin, ok := raw.(codexTurnStateOrigin)
	if !ok {
		codexTurnStateOrigins.Delete(affinityKey)
		return
	}
	if !origin.expiresAt.IsZero() && time.Now().After(origin.expiresAt) {
		codexTurnStateOrigins.Delete(affinityKey)
		return
	}
	if origin.accountID != account.ID() {
		headers.Del(codexTurnStateHeader)
	}
}

type codexTurnStateAffinityContextKey struct{}

// WithCodexTurnStateAffinityKey attaches the session affinity key used by
// turn-state echo guarding so WebSocket executors can read it from ctx.
func WithCodexTurnStateAffinityKey(ctx context.Context, affinityKey string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, codexTurnStateAffinityContextKey{}, strings.TrimSpace(affinityKey))
}

// CodexTurnStateAffinityKeyFromContext returns the affinity key set by
// WithCodexTurnStateAffinityKey, or "" when absent.
func CodexTurnStateAffinityKeyFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(codexTurnStateAffinityContextKey{}).(string)
	return v
}

// GuardCodexTurnStateEcho strips client-echoed turn-state known to have been
// minted by a different account for this session. Safe no-op when affinityKey
// is empty or provenance is missing.
func GuardCodexTurnStateEcho(affinityKey string, account *auth.Account, headers http.Header) {
	guardCodexTurnStateEcho(affinityKey, account, headers)
}

// NoteCodexTurnStateProvenance records which account minted turn-state for
// affinityKey. Exported so WS-path tests can seed provenance.
func NoteCodexTurnStateProvenance(affinityKey string, account *auth.Account) {
	noteCodexTurnStateProvenance(affinityKey, account)
}

// ClearCodexTurnStateProvenance removes a provenance entry (tests / cleanup).
func ClearCodexTurnStateProvenance(affinityKey string) {
	if strings.TrimSpace(affinityKey) == "" {
		return
	}
	codexTurnStateOrigins.Delete(affinityKey)
}

func noteCodexTurnStateProvenance(affinityKey string, account *auth.Account) {
	if strings.TrimSpace(affinityKey) == "" || account == nil || account.ID() <= 0 {
		return
	}
	codexTurnStateOrigins.Store(affinityKey, codexTurnStateOrigin{
		accountID: account.ID(),
		expiresAt: time.Now().Add(codexTurnStateProvenanceTTL),
	})
	sweepCodexTurnStateOrigins()
}

// sweepCodexTurnStateOrigins is an opportunistic sweep: every 256 writes it runs a full pass, to prevent a slow leak caused by
// lazy deletion on the read side alone (session keys are unbounded).
func sweepCodexTurnStateOrigins() {
	if codexTurnStateWrites.Add(1)%256 != 0 {
		return
	}
	now := time.Now()
	live := 0
	codexTurnStateOrigins.Range(func(key, value any) bool {
		origin, ok := value.(codexTurnStateOrigin)
		if !ok || (!origin.expiresAt.IsZero() && now.After(origin.expiresAt)) {
			codexTurnStateOrigins.Delete(key)
			return true
		}
		live++
		return true
	})
	if live <= codexTurnStateMaxOrigins {
		return
	}
	// When over the limit, arbitrary entries are dropped. A missing origin only lets the guard pass one echo through and does not affect
	// forwarding correctness; this is safer than letting memory keep growing with the number of session keys.
	excess := live - codexTurnStateMaxOrigins
	codexTurnStateOrigins.Range(func(key, _ any) bool {
		if excess <= 0 {
			return false
		}
		codexTurnStateOrigins.Delete(key)
		excess--
		return true
	})
}
