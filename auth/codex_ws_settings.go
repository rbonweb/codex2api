package auth

import "github.com/codex2api/database"

// SetCodexWSDownstreamKeepaliveSlots sets the per-account cap on idle chat connections kept alive.
func (s *Store) SetCodexWSDownstreamKeepaliveSlots(slots int) {
	if s != nil {
		s.codexWSDownstreamKeepaliveSlots.Store(int64(database.NormalizeCodexWSDownstreamKeepaliveSlots(slots)))
	}
}

// CodexWSDownstreamKeepaliveSlots returns the per-account cap on idle chat connections kept alive.
func (s *Store) CodexWSDownstreamKeepaliveSlots() int {
	if s == nil {
		return database.DefaultCodexWSDownstreamKeepaliveSlots
	}
	return database.NormalizeCodexWSDownstreamKeepaliveSlots(int(s.codexWSDownstreamKeepaliveSlots.Load()))
}
