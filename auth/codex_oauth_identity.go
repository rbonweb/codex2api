package auth

import (
	"context"
	"net/http"
)

// Injected by startup wiring and shares the resolver with conversations, so that auth does not depend on proxy in reverse.
var CodexOAuthIdentityHeaders func(*Account) (http.Header, error)

type codexOAuthIdentityKey struct{}

type codexOAuthIdentity struct {
	headers http.Header
	reason  string
}

func withCodexOAuthIdentity(ctx context.Context, account *Account, force bool) (context.Context, error) {
	identity := codexOAuthIdentity{reason: "proactive_expiry"}
	if force {
		identity.reason = "forced"
	}
	if CodexOAuthIdentityHeaders != nil {
		headers, err := CodexOAuthIdentityHeaders(account)
		if err != nil {
			return nil, err
		}
		identity.headers = headers.Clone()
	}
	return context.WithValue(ctx, codexOAuthIdentityKey{}, identity), nil
}

func applyCodexOAuthIdentity(req *http.Request) {
	identity, _ := req.Context().Value(codexOAuthIdentityKey{}).(codexOAuthIdentity)
	// OAuth refresh uses only the default client identity and carries no conversation, device attestation, or account auth headers.
	for _, name := range []string{"User-Agent", "Originator"} {
		if value := identity.headers.Get(name); value != "" {
			req.Header.Set(name, value)
		}
	}
}

func codexOAuthRefreshReason(ctx context.Context) string {
	identity, ok := ctx.Value(codexOAuthIdentityKey{}).(codexOAuthIdentity)
	if ok {
		return identity.reason
	}
	return "forced"
}
