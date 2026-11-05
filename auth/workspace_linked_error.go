package auth

import (
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"time"
)

const workspaceLinkedErrorDedupTTL = 60 * time.Second

type workspaceLinkedMark struct {
	until     time.Time
	triggerID int64
}

// MarkDeactivatedWorkspace marks the triggering account as error, and in the same workspace
// (native AccountID / chatgpt_account_id) it also isolates the other Codex accounts.
// When a ChatGPT Team / K12 workspace is deactivated, its seats fail together; marking only the triggering account would let scheduling keep hitting its sibling accounts.
//
// The grouping key is the workspace the request actually hit (EffectiveAccountID, including the account-level Chatgpt-Account-Id override).
// Sibling accounts are matched first by native AccountID; Team/K12/edu seats are then matched by effective workspace
// (each person may have a separate AccountID, but traffic goes to the same space).
// A personal account only overridden to that space by a request header is not killed as a whole account.
// It is on by default, with no config switch.
func (s *Store) MarkDeactivatedWorkspace(trigger *Account, errorMsg string) {
	if s == nil || trigger == nil {
		return
	}
	targets := s.workspaceLinkedTargets(trigger)
	for _, acc := range targets {
		atomic.StoreInt32(&acc.Disabled, 1)
	}
	s.MarkError(trigger, errorMsg)
	if len(targets) == 0 {
		return
	}
	linkedMsg := deactivatedWorkspaceLinkedMessage(trigger.DBID)
	for _, acc := range targets {
		s.MarkError(acc, linkedMsg)
	}
	log.Printf("account %d workspace deactivated, linked-marking %d same-workspace accounts", trigger.DBID, len(targets))
}

func deactivatedWorkspaceLinkedMessage(triggerID int64) string {
	return fmt.Sprintf("upstream returned: deactivated_workspace (workspace-linked, triggered by account %d)", triggerID)
}

func (s *Store) workspaceLinkedTargets(trigger *Account) []*Account {
	workspaceID := strings.TrimSpace(trigger.EffectiveAccountID())
	if workspaceID == "" {
		return nil
	}
	if !s.markWorkspaceLinkedFired(workspaceID, trigger.DBID) {
		return nil
	}
	var targets []*Account
	for _, acc := range s.Accounts() {
		if !shouldLinkDeactivatedWorkspace(trigger, acc, workspaceID) {
			continue
		}
		targets = append(targets, acc)
	}
	return targets
}

func shouldLinkDeactivatedWorkspace(trigger, sibling *Account, workspaceID string) bool {
	if sibling == nil || trigger == nil || sibling.DBID == trigger.DBID {
		return false
	}
	if siblingErrorStatus(sibling) {
		return false
	}
	if nativeWorkspaceID(sibling) == workspaceID {
		return true
	}
	// The native AccountID of a Team/K12 seat may be a personal seat, but WHAM and traffic go to the same workspace.
	return isWorkspaceSeatPlan(sibling) && strings.TrimSpace(sibling.EffectiveAccountID()) == workspaceID
}

func isWorkspaceSeatPlan(acc *Account) bool {
	if acc == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(acc.GetPlanType())) {
	case "team", "teamplus", "k12", "edu", "education":
		return true
	default:
		return false
	}
}

func nativeWorkspaceID(acc *Account) string {
	if acc == nil {
		return ""
	}
	acc.mu.RLock()
	defer acc.mu.RUnlock()
	return strings.TrimSpace(acc.AccountID)
}

func siblingErrorStatus(acc *Account) bool {
	if acc == nil {
		return false
	}
	acc.mu.RLock()
	defer acc.mu.RUnlock()
	return acc.Status == StatusError
}

// LinkedDeactivatedWorkspaceResult lets batch tests short-circuit before hitting WHAM:
// the account was already marked as error because its space was deactivated, or its workspace was just deactivated.
func (s *Store) LinkedDeactivatedWorkspaceResult(acc *Account) (string, bool) {
	if s == nil || acc == nil {
		return "", false
	}
	acc.mu.RLock()
	status := acc.Status
	errMsg := acc.ErrorMsg
	acc.mu.RUnlock()
	if status == StatusError && strings.Contains(strings.ToLower(errMsg), "deactivated_workspace") {
		return errMsg, true
	}
	workspaceID := strings.TrimSpace(acc.EffectiveAccountID())
	nativeID := nativeWorkspaceID(acc)
	triggerID, ok := s.lookupDeactivatedWorkspace(workspaceID, nativeID)
	if !ok {
		return "", false
	}
	if nativeID != workspaceID && !isWorkspaceSeatPlan(acc) {
		return "", false
	}
	return deactivatedWorkspaceLinkedMessage(triggerID), true
}

func (s *Store) lookupDeactivatedWorkspace(ids ...string) (int64, bool) {
	now := time.Now()
	s.workspaceLinkedMu.Lock()
	defer s.workspaceLinkedMu.Unlock()
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if mark, ok := s.workspaceLinkedRecent[id]; ok && mark.until.After(now) {
			return mark.triggerID, true
		}
	}
	return 0, false
}

// markWorkspaceLinkedFired deduplicates in-process, keyed by workspaceID: within the TTL, only one fan-out is allowed per workspace.
func (s *Store) markWorkspaceLinkedFired(workspaceID string, triggerID int64) bool {
	now := time.Now()
	s.workspaceLinkedMu.Lock()
	defer s.workspaceLinkedMu.Unlock()
	if mark, ok := s.workspaceLinkedRecent[workspaceID]; ok && mark.until.After(now) {
		return false
	}
	if s.workspaceLinkedRecent == nil {
		s.workspaceLinkedRecent = make(map[string]workspaceLinkedMark)
	}
	for k, mark := range s.workspaceLinkedRecent {
		if !mark.until.After(now) {
			delete(s.workspaceLinkedRecent, k)
		}
	}
	s.workspaceLinkedRecent[workspaceID] = workspaceLinkedMark{
		until:     now.Add(workspaceLinkedErrorDedupTTL),
		triggerID: triggerID,
	}
	return true
}
