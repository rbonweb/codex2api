package proxy

import (
	"context"
	"io"
	"net/http"
	"sync"

	"github.com/codex2api/security/outputguard"
)

const (
	// GuardTriggeredPublicCode is the stable public error code; the internal trigger reason
	//(repetition/channel_bytes/...) must not leak downstream.
	GuardTriggeredPublicCode = "model_output_guard_triggered"
	// GuardTriggeredPublicMessage is the redacted public terminal message.
	GuardTriggeredPublicMessage = "Upstream model output exceeded the gateway safety policy; generation was stopped."
)

// GuardTerminationCoordinator orchestrates forced termination after the output guard trips:
//  1. The first action is to cancel the upstream context proactively, before waiting for any downstream write lock;
//  2. close the upstream response body, skipping the 5s drain;
//  3. discard local stream buffers, so that later Flush/Finalize cannot release bytes already judged runaway.
//
// sync.Once guarantees that the above actions run atomically and only once under concurrent triggers, and freezes the first
// trigger reason, so that a later reason cannot overwrite an existing terminal state.
type GuardTerminationCoordinator struct {
	guard          *outputguard.RequestGuard
	cancelUpstream context.CancelFunc
	body           io.Closer
	once           sync.Once
	trippedOutcome streamOutcome
}

// NewGuardTerminationCoordinator constructs the termination coordinator. cancelUpstream and body may both be nil
// (for example, when the upstream has not yet established a connection), in which case the corresponding steps are skipped automatically.
func NewGuardTerminationCoordinator(guard *outputguard.RequestGuard, cancelUpstream context.CancelFunc, body io.Closer) *GuardTerminationCoordinator {
	return &GuardTerminationCoordinator{guard: guard, cancelUpstream: cancelUpstream, body: body}
}

// HandleTripped performs forced termination once (and only once) and returns the frozen guard terminal state.
// discardBuffers is usually a buffer-clearing callback such as streamFlushWriter.DiscardBuffered;
// the callback runs inside once, so concurrent callers only ever observe the same outcome that was written last.
func (c *GuardTerminationCoordinator) HandleTripped(reason string, discardBuffers ...func()) streamOutcome {
	if c == nil {
		return guardTerminalOutcome(reason)
	}
	c.once.Do(func() {
		// First cancel the upstream and close the response body, then do any local cleanup. The downstream write lock and notifications are not inside this function,
		// so lock contention can never delay upstream cancellation or resource reclamation.
		if c.cancelUpstream != nil {
			c.cancelUpstream()
		}
		if c.body != nil {
			_ = c.body.Close()
		}
		for _, discard := range discardBuffers {
			if discard != nil {
				discard()
			}
		}
		frozenReason := reason
		if c.guard != nil && c.guard.IsTripped() && c.guard.TrippedReason() != "" {
			frozenReason = c.guard.TrippedReason()
		}
		c.trippedOutcome = guardTerminalOutcome(frozenReason)
	})
	return c.trippedOutcome
}

// guardTerminalOutcome builds the guard terminal state, which takes absolute precedence: 502, model_output_guard,
// does not penalize the account, is a local terminal state, and carries no raw failure payload.
func guardTerminalOutcome(reason string) streamOutcome {
	return streamOutcome{
		logStatusCode:  http.StatusBadGateway,
		failureKind:    "model_output_guard",
		failureMessage: GuardTriggeredPublicMessage,
		guardTripped:   true,
		guardReason:    reason,
		penalize:       false,
		terminalLocal:  true,
	}
}
