package auth

// SetAccountsForTest directly replaces the in-memory account list; it is only for tests in other packages.
func (s *Store) SetAccountsForTest(accounts []*Account) {
	s.mu.Lock()
	s.accounts = append([]*Account(nil), accounts...)
	s.mu.Unlock()
}
