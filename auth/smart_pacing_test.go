package auth

import (
	"testing"
	"time"
)

func TestSmartPacingRatio(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name      string
		usage     float64
		valid     bool
		resetAt   time.Time
		window    time.Duration
		wantOK    bool
		wantThrot bool // expected ratio < 1 (throttling needed)
	}{
		{
			name:    "invalid usage signal does not intervene",
			usage:   90,
			valid:   false,
			resetAt: now.Add(time.Hour),
			window:  smartPacingWindow5h,
			wantOK:  false,
		},
		{
			name:    "unknown reset time does not intervene",
			usage:   90,
			valid:   true,
			resetAt: time.Time{},
			window:  smartPacingWindow5h,
			wantOK:  false,
		},
		{
			name:    "window already rolled over does not intervene",
			usage:   90,
			valid:   true,
			resetAt: now.Add(-time.Minute),
			window:  smartPacingWindow5h,
			wantOK:  false,
		},
		{
			name:    "exhausted, leave it to the rate-limit logic",
			usage:   100,
			valid:   true,
			resetAt: now.Add(time.Hour),
			window:  smartPacingWindow5h,
			wantOK:  false,
		},
		{
			name:      "burning too fast, needs throttling",
			usage:     95, // 5% left, but 1h remaining (should pace evenly to the reset)
			valid:     true,
			resetAt:   now.Add(time.Hour),
			window:    smartPacingWindow5h,
			wantOK:    true,
			wantThrot: true,
		},
		{
			name:      "exactly even pace, no throttling",
			usage:     50, // 50% left, 2.5h left (half the window), exactly even pace
			valid:     true,
			resetAt:   now.Add(150 * time.Minute),
			window:    smartPacingWindow5h,
			wantOK:    true,
			wantThrot: false,
		},
		{
			name:      "behind schedule, no throttling",
			usage:     10,
			valid:     true,
			resetAt:   now.Add(4 * time.Hour),
			window:    smartPacingWindow5h,
			wantOK:    true,
			wantThrot: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ratio, ok := smartPacingRatio(tc.usage, tc.valid, tc.resetAt, tc.window, now)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (ratio=%.4f)", ok, tc.wantOK, ratio)
			}
			if !ok {
				return
			}
			if tc.wantThrot && ratio >= 1 {
				t.Fatalf("ratio = %.4f, want < 1 (throttling needed)", ratio)
			}
			if !tc.wantThrot && ratio < 1 {
				t.Fatalf("ratio = %.4f, want >= 1 (no throttling)", ratio)
			}
		})
	}
}

func TestSmartPacingConcurrencyLimitLocked(t *testing.T) {
	now := time.Now()

	newOverBurnAccount := func() *Account {
		return &Account{
			smartPacingEnabled:        true,
			smartPacingMinConcurrency: 1,
			smartPacingWindows5h:      true,
			smartPacingWindows7d:      true,
			UsagePercent5h:            95, // 5% left
			UsagePercent5hValid:       true,
			Reset5hAt:                 now.Add(time.Hour), // 1h left, ratio≈0.25
		}
	}

	t.Run("no change when disabled", func(t *testing.T) {
		a := newOverBurnAccount()
		a.smartPacingEnabled = false
		if got := a.smartPacingConcurrencyLimitLocked(8, now); got != 8 {
			t.Fatalf("got %d, want 8 (disabled should not throttle)", got)
		}
	})

	t.Run("burning too fast, concurrency scaled down proportionally", func(t *testing.T) {
		a := newOverBurnAccount()
		// ratio = 5*18000/(100*3600) = 0.25 → ceil(8*0.25)=2
		if got := a.smartPacingConcurrencyLimitLocked(8, now); got != 2 {
			t.Fatalf("got %d, want 2", got)
		}
	})

	t.Run("not below the concurrency floor", func(t *testing.T) {
		a := newOverBurnAccount()
		a.smartPacingMinConcurrency = 3
		if got := a.smartPacingConcurrencyLimitLocked(8, now); got != 3 {
			t.Fatalf("got %d, want 3 (floor protection)", got)
		}
	})

	t.Run("no change when floor >= limit", func(t *testing.T) {
		a := newOverBurnAccount()
		a.smartPacingMinConcurrency = 8
		if got := a.smartPacingConcurrencyLimitLocked(8, now); got != 8 {
			t.Fatalf("got %d, want 8", got)
		}
	})

	t.Run("limit<=1 no change", func(t *testing.T) {
		a := newOverBurnAccount()
		if got := a.smartPacingConcurrencyLimitLocked(1, now); got != 1 {
			t.Fatalf("got %d, want 1", got)
		}
	})

	t.Run("no change at even pace", func(t *testing.T) {
		a := newOverBurnAccount()
		a.UsagePercent5h = 50
		a.Reset5hAt = now.Add(150 * time.Minute)
		if got := a.smartPacingConcurrencyLimitLocked(8, now); got != 8 {
			t.Fatalf("got %d, want 8 (even pace, no throttling)", got)
		}
	})

	t.Run("only paces the selected window", func(t *testing.T) {
		a := newOverBurnAccount()
		a.smartPacingWindows5h = false // only look at 7d, and the 7d signal is invalid
		a.UsagePercent7dValid = false
		if got := a.smartPacingConcurrencyLimitLocked(8, now); got != 8 {
			t.Fatalf("got %d, want 8 (5h not selected and 7d invalid)", got)
		}
	})

	t.Run("takes the stricter window", func(t *testing.T) {
		a := newOverBurnAccount()
		// 5h at even pace (ratio=1), 7d burning too fast (ratio≈0.25) → take 7d
		a.UsagePercent5h = 50
		a.Reset5hAt = now.Add(150 * time.Minute)
		a.UsagePercent7d = 95
		a.UsagePercent7dValid = true
		a.Reset7dAt = now.Add(100 * time.Hour) // 5% left, but the 7d window has 100h left → burning far ahead of pace
		got := a.smartPacingConcurrencyLimitLocked(8, now)
		if got >= 8 || got < 1 {
			t.Fatalf("got %d, want it capped to [1,8) by the 7d window", got)
		}
	})

	// The team monthly window paces on its real 30-day cycle, not a fixed 7 days: with the same usage and remaining time,
	// treating it as 7 days would over-throttle, while the real monthly window judges it as even pace with no throttling.
	t.Run("team monthly window paces on its real cycle", func(t *testing.T) {
		newTeamMonthly := func() *Account {
			return &Account{
				smartPacingEnabled:        true,
				smartPacingMinConcurrency: 1,
				smartPacingWindows5h:      false, // isolate, look only at the long window
				smartPacingWindows7d:      true,
				UsagePercent7d:            50,
				UsagePercent7dValid:       true,
				Reset7dAt:                 now.Add(15 * 24 * time.Hour), // 30-day window half elapsed, usage half used → even pace
			}
		}

		// Known monthly window length (30 days = 2592000s): ratio=1.0 → no throttling.
		a := newTeamMonthly()
		a.Window7dSeconds = 2_592_000
		if got := a.smartPacingConcurrencyLimitLocked(8, now); got != 8 {
			t.Fatalf("monthly window on its real cycle should judge even pace with no throttling, got %d want 8", got)
		}

		// Unknown length (=0) falls back to a fixed 7 days: the same data would be misjudged as burning ahead of pace and cut concurrency,
		// confirming that without fixing the classification, team accounts would be over-throttled.
		b := newTeamMonthly()
		if got := b.smartPacingConcurrencyLimitLocked(8, now); got >= 8 {
			t.Fatalf("falling back to 7 days should over-throttle (proves the gap), got %d want <8", got)
		}
	})
}
