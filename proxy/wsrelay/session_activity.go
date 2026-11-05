package wsrelay

import "time"

// Touch updates the last business-activity time, and permanently marks the session as having carried user chat.
func (s *Session) Touch() {
	s.touchActivity(true)
}

func (s *Session) touchActivity(userContext bool) {
	s.mu.Lock()
	s.LastActiveAt = time.Now()
	s.userContext = s.userContext || userContext
	s.mu.Unlock()
}

func (s *Session) hasUserContext() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.userContext
}

func (s *Session) markUserContext() {
	s.mu.Lock()
	s.userContext = true
	s.mu.Unlock()
}

// IsExpired checks the business idle timeout; a blank session keeps the connection in normal mode.
func (s *Session) IsExpired() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.userContext && !weakNetworkModeEnabled() {
		return false
	}
	return time.Since(s.LastActiveAt) >= connectionIdleTimeout()
}
