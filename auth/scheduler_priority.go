package auth

// Account scheduling priority (issue #358): accounts with higher priority are always scheduled first; within the same priority,
// health tier and scheduling score compete; when an account is unavailable (cooling down / paused / limited), traffic naturally falls back to lower priority.
// Typical use: give official accounts a positive value, and leave API-key relay channels at the default or set a negative value, to implement
// the fallback arrangement of "relay only once official accounts are used up".
const (
	minSchedulerPriority int64 = -100
	maxSchedulerPriority int64 = 100
)

func normalizeSchedulerPriority(priority int64) int64 {
	switch {
	case priority < minSchedulerPriority:
		return minSchedulerPriority
	case priority > maxSchedulerPriority:
		return maxSchedulerPriority
	default:
		return priority
	}
}

func (a *Account) SetSchedulerPriority(priority int64) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.SchedulerPriority = normalizeSchedulerPriority(priority)
	a.mu.Unlock()
}

func (a *Account) schedulerPriority() int64 {
	if a == nil {
		return 0
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return normalizeSchedulerPriority(a.SchedulerPriority)
}

func (a *Account) GetSchedulerPriority() int64 {
	return a.schedulerPriority()
}

// ApplyAccountSchedulerPriority dynamically updates an account's scheduling priority (nil restores the default 0).
func (s *Store) ApplyAccountSchedulerPriority(dbID int64, priority *int64) bool {
	acc := s.FindByID(dbID)
	if acc == nil {
		return false
	}
	if priority == nil {
		acc.SetSchedulerPriority(0)
	} else {
		acc.SetSchedulerPriority(*priority)
	}
	s.fastSchedulerUpdate(acc)
	return true
}
