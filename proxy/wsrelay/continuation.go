package wsrelay

import (
	"context"
	"log"
	"time"

	"github.com/codex2api/proxy"
)

// acquireContinuation only waits for the original connection; after the binding expires, the caller restores the full context.
func (e *Executor) acquireContinuation(ctx context.Context, input websocketContinuation) (*WsConnection, *PendingRequest, string, error) {
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, busyAcquireMaxWait())
	defer cancel()
	start, waited := time.Now(), false
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, "", err
		}
		wake := e.manager.accountWaitSignal(input.accountID)
		wc, _ := e.manager.lookupContinuationConn(input)
		if wc == nil {
			return nil, nil, "", &proxy.ResponsesContinuationLostError{Reason: "original_connection_unavailable"}
		}
		connection, pending, key := e.manager.acquirePreferredConnection(input)
		if connection != nil {
			if waited {
				log.Printf("[WS] reused the original connection after waiting for continuation account=%d waited_ms=%d", input.accountID, time.Since(start).Milliseconds())
			}
			return connection, pending, key, nil
		}
		waited = true
		if _, err := waitForAccountChange(ctx, wake, AcquireMaxBackoff); err != nil {
			if parent.Err() == nil && ctx.Err() == context.DeadlineExceeded {
				return nil, nil, "", &proxy.ResponsesContinuationBusyError{}
			}
			return nil, nil, "", err
		}
	}
}

func (r *WsResponse) bindCompletedResponse(responseID string) {
	if responseID == "" || r.manager == nil || r.conn == nil || r.conn.session == nil {
		return
	}
	r.manager.bindResponseConn(responseID, responseConnBinding{conn: r.conn, sessionKey: r.sessionID,
		accountID: r.conn.session.AccountID, apiKey: r.apiKey, model: r.model})
}

func (m *Manager) lookupContinuationConn(input websocketContinuation) (*WsConnection, string) {
	wc, key := m.lookupResponseConn(input.responseID, input.accountID, input.apiKey)
	if wc == nil || (input.identity != "" && wc.upstreamClientIdentity != input.identity) {
		return nil, ""
	}
	if input.url != "" && wc.PoolKey != m.poolKey(input.accountID, input.url, key, input.proxyURL) {
		return nil, ""
	}
	m.respConnMu.Lock()
	binding, exists := m.respConnBindings[input.responseID]
	m.respConnMu.Unlock()
	if !exists || binding.conn != wc || (binding.model != "" && input.model != "" && binding.model != input.model) {
		return nil, ""
	}
	return wc, key
}

// After a send failure, back off in a cancellable way to avoid a reconnect storm.
func waitWebsocketSendRetry(ctx context.Context, retry int) error {
	const sendRetryBackoff = 200 * time.Millisecond
	timer := time.NewTimer(time.Duration(retry+1) * sendRetryBackoff)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
