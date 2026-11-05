package database

const (
	DefaultCodexWSDownstreamKeepaliveSlots = 8
	MaxCodexWSDownstreamKeepaliveSlots     = 32
)

// NormalizeCodexWSDownstreamKeepaliveSlots normalizes the idle chat connection retention cap; 0 means no connections are retained.
func NormalizeCodexWSDownstreamKeepaliveSlots(slots int) int {
	if slots < 0 {
		return DefaultCodexWSDownstreamKeepaliveSlots
	}
	if slots > MaxCodexWSDownstreamKeepaliveSlots {
		return MaxCodexWSDownstreamKeepaliveSlots
	}
	return slots
}
