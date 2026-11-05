package wsrelay

import "sort"

// The caller holds the account lock. Reasoning, dialing, and connections that do not yet carry chat do not take up the idle retention quota.
func (m *Manager) trimIdleChatConnections(input connectionCapacityRequest) {
	input.kind = capacityChat
	idle := m.idleCapacityConnections(input)
	sort.Slice(idle, func(i, j int) bool {
		return idle[i].lastUsed < idle[j].lastUsed
	})
	excess := len(idle) - input.limit
	for _, candidate := range idle {
		if excess <= 0 {
			return
		}
		if m.discardIdleChatConnection(candidate.wc) {
			excess--
		}
	}
}

// Even if a continuation binding exists, it can be reclaimed by LRU; the account lock prevents a candidate from being taken by new inference at the same time.
func (m *Manager) discardIdleChatConnection(wc *WsConnection) bool {
	m.respConnMu.Lock()
	current, exists := m.connections.Load(wc.PoolKey)
	if !exists || current != wc || wc.session.PendingCount() != 0 {
		m.respConnMu.Unlock()
		return false
	}
	wc.responseBindingsDisabled = true
	m.respConnMu.Unlock()
	m.discardConnectionFor(wc, closeIdleChatLRU)
	return true
}

func (m *Manager) retainReleasedConnection(wc *WsConnection) {
	lock, release := m.accountLock(wc.session.AccountID)
	defer release()
	lock.Lock()
	defer lock.Unlock()
	current, exists := m.connections.Load(wc.PoolKey)
	if !exists || current != wc || !wc.IsConnected() {
		return
	}
	if connectionBudgetKind(wc) == capacityBlank && connectionLimit(capacityBlank) == 0 && wc.session.PendingCount() == 0 {
		m.discardConnectionFor(wc, closeCapacityReclaim)
		return
	}
	wc.Touch()
	input := connectionCapacityRequest{accountID: wc.session.AccountID, limit: connectionLimit(capacityChat)}
	m.trimIdleChatConnections(input)
}

// After a smaller setting is saved, the background check shrinks the idle retention pool even without new requests.
func (m *Manager) trimAllIdleChatConnections() {
	accounts := make(map[int64]struct{})
	m.connections.Range(func(_, value any) bool {
		wc := value.(*WsConnection)
		if wc.session != nil {
			accounts[wc.session.AccountID] = struct{}{}
		}
		return true
	})
	for accountID := range accounts {
		m.trimAccountIdleChatConnections(accountID)
	}
}

func (m *Manager) trimAccountIdleChatConnections(accountID int64) {
	lock, release := m.accountLock(accountID)
	defer release()
	lock.Lock()
	defer lock.Unlock()
	m.trimIdleChatConnections(connectionCapacityRequest{accountID: accountID, limit: connectionLimit(capacityChat)})
	if connectionLimit(capacityBlank) == 0 {
		m.reclaimConnectionCapacity(connectionCapacityRequest{accountID: accountID, kind: capacityBlank})
	}
}
