package auth

// withMaxConcurrency lets tests build a Store: maxConcurrency is an atomic.Int64 and cannot be set in a composite literal.
func (s *Store) withMaxConcurrency(n int64) *Store {
	s.maxConcurrency.Store(n)
	return s
}
