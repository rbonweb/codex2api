// database/acceptance_config_test.go
package database

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

// Scenario 21: migration is off by default (persistence is explicitly enabled or disabled), concurrent unrelated saves lose no fields, and generation overflow rolls back
func TestAcceptanceScenario21_DBCurrencyAndRestoreDefaults(t *testing.T) {
	drivers := []string{"sqlite", "postgres"}
	pgDSN := os.Getenv("CODEX2API_TEST_POSTGRES_DSN")

	for _, driver := range drivers {
		t.Run(driver, func(t *testing.T) {
			var db *DB
			var err error
			if driver == "sqlite" {
				db, err = New("sqlite", filepath.Join(t.TempDir(), "acceptance_outputguard.db"))
			} else {
				// Build with the real PostgreSQL driver. Skip when there is no DSN and report that honestly rather than claiming it was tested
				if pgDSN == "" {
					t.Skip("PostgreSQL DSN not configured; skipped")
				}
				db, err = New("postgres", pgDSN)
			}
			if err != nil {
				t.Fatalf("New(%s) error = %v", driver, err)
			}
			t.Cleanup(func() { _ = db.Close() })

			ctx := context.Background()

			// 1. Fresh database default check (tri-state pointers default to off/false; the 5 budget values and the ConfigGeneration default are checked exactly)
			settings, err := db.GetOutputGuardSettings(ctx)
			if err != nil {
				t.Fatalf("GetOutputGuardSettings error = %v", err)
			}
			if settings.Enabled == nil || *settings.Enabled != false {
				t.Fatalf("expected Enabled=false by default, got %v", settings.Enabled)
			}
			if settings.MaxToolArgumentBytes != 262144 || settings.MaxToolArgumentDeltas != 4096 ||
				settings.MaxChannelBytes != 2097152 || settings.MaxRequestBytes != 4194304 ||
				settings.ConfigGeneration != 1 {
				t.Fatalf("unexpected defaults: %+v", settings)
			}

			// 2. Write an explicit false, assert it persists and is not overwritten by a zero value after restart; NULL fallback is preserved
			falseVal := false
			updated, err := db.UpdateOutputGuardSettings(ctx, OutputGuardSettingsUpdate{
				Enabled: &falseVal,
			})
			if err != nil {
				t.Fatalf("update to false error = %v", err)
			}
			if updated.Enabled == nil || *updated.Enabled != false {
				t.Fatalf("expected Enabled=false after update, got %v", updated.Enabled)
			}

			// Reopen the DB connection and verify the explicit false is correctly restored from persistence
			if driver == "sqlite" {
				dbPath := filepath.Join(t.TempDir(), "acceptance_restart.db")
				initDB, ierr := New("sqlite", dbPath)
				if ierr != nil {
					t.Fatalf("New(sqlite) error = %v", ierr)
				}
				if _, uerr := initDB.UpdateOutputGuardSettings(ctx, OutputGuardSettingsUpdate{Enabled: &falseVal}); uerr != nil {
					t.Fatalf("setup explicit false error = %v", uerr)
				}
				_ = initDB.Close()

				restartedDB, rerr := New("sqlite", dbPath)
				if rerr != nil {
					t.Fatalf("reopen error = %v", rerr)
				}
				t.Cleanup(func() { _ = restartedDB.Close() })

				readBack, gerr := restartedDB.GetOutputGuardSettings(ctx)
				if gerr != nil || readBack.Enabled == nil || *readBack.Enabled != false {
					t.Fatalf("expected persistent false after restart, got %v", readBack.Enabled)
				}
			}

			// Verify NULL fallback: set output_guard_enabled to NULL, Get must fill in the default false
			if _, err := db.conn.ExecContext(ctx, `UPDATE system_settings SET output_guard_enabled = NULL WHERE id = 1`); err != nil {
				t.Fatalf("seed NULL enabled failed: %v", err)
			}
			nullFallbackSettings, err := db.GetOutputGuardSettings(ctx)
			if err != nil || nullFallbackSettings.Enabled == nil || *nullFallbackSettings.Enabled != false {
				t.Fatalf("expected NULL fallback to false, got: %+v", nullFallbackSettings)
			}

			// Restore to explicit true
			trueVal := true
			restoredTrue, err := db.UpdateOutputGuardSettings(ctx, OutputGuardSettingsUpdate{
				Enabled: &trueVal,
			})
			if err != nil || restoredTrue.Enabled == nil || *restoredTrue.Enabled != true {
				t.Fatalf("restore to true error = %v", err)
			}

			// 3. Scenario 21 gap 1: invalid config (0 / negative / out of range / tool>channel / channel>request) is all rejected, and the full settings + generation read before and after is strictly unchanged
			beforeInvalid, err := db.GetOutputGuardSettings(ctx)
			if err != nil {
				t.Fatalf("get settings before invalid: %v", err)
			}
			invalidCases := []struct {
				name   string
				update OutputGuardSettingsUpdate
			}{
				{"tool zero", OutputGuardSettingsUpdate{MaxToolArgumentBytes: ptr(int64(0))}},
				{"tool negative", OutputGuardSettingsUpdate{MaxToolArgumentBytes: ptr(int64(-1))}},
				{"tool < 16KiB", OutputGuardSettingsUpdate{MaxToolArgumentBytes: ptr(int64(16383))}},
				{"tool > 4MiB", OutputGuardSettingsUpdate{MaxToolArgumentBytes: ptr(int64(4194305))}},
				{"deltas zero", OutputGuardSettingsUpdate{MaxToolArgumentDeltas: ptr(0)}},
				{"deltas negative", OutputGuardSettingsUpdate{MaxToolArgumentDeltas: ptr(-5)}},
				{"deltas < 256", OutputGuardSettingsUpdate{MaxToolArgumentDeltas: ptr(255)}},
				{"deltas > 65536", OutputGuardSettingsUpdate{MaxToolArgumentDeltas: ptr(65537)}},
				{"channel zero", OutputGuardSettingsUpdate{MaxChannelBytes: ptr(int64(0))}},
				{"channel < 128KiB", OutputGuardSettingsUpdate{MaxChannelBytes: ptr(int64(131071))}},
				{"channel > 16MiB", OutputGuardSettingsUpdate{MaxChannelBytes: ptr(int64(16777217))}},
				{"request zero", OutputGuardSettingsUpdate{MaxRequestBytes: ptr(int64(0))}},
				{"request < 256KiB", OutputGuardSettingsUpdate{MaxRequestBytes: ptr(int64(262143))}},
				{"request > 64MiB", OutputGuardSettingsUpdate{MaxRequestBytes: ptr(int64(67108865))}},
				{"tool > channel", OutputGuardSettingsUpdate{MaxToolArgumentBytes: ptr(int64(3000000)), MaxChannelBytes: ptr(int64(2097152))}},
				{"channel > request", OutputGuardSettingsUpdate{MaxChannelBytes: ptr(int64(5000000)), MaxRequestBytes: ptr(int64(4194304))}},
			}
			for _, tc := range invalidCases {
				_, uerr := db.UpdateOutputGuardSettings(ctx, tc.update)
				if !errors.Is(uerr, ErrInvalidOutputGuardSettings) {
					t.Fatalf("[%s] expected ErrInvalidOutputGuardSettings, got %v", tc.name, uerr)
				}
			}
			afterInvalid, err := db.GetOutputGuardSettings(ctx)
			if err != nil {
				t.Fatalf("get settings after invalid: %v", err)
			}
			if !reflect.DeepEqual(beforeInvalid, afterInvalid) {
				t.Fatalf("settings changed after rejected invalid updates! before=%+v, after=%+v", beforeInvalid, afterInvalid)
			}

			// 4. Scenario 21 gap 2: exact check of final values for concurrent narrow updates: two disjoint guard field updates run concurrently, plus the response cache settings group updated concurrently on its own
			// Collect each goroutine's error and never ignore it; verify each field's committed value and the real revision increment
			genBeforeConcurrent := afterInvalid.ConfigGeneration
			const guardGoroutines = 4
			const rcGoroutines = 4
			var wg sync.WaitGroup
			guardErrCh := make(chan error, guardGoroutines)
			rcErrCh := make(chan error, rcGoroutines)

			// Build the two updates over disjoint field sets separately:
			// G1, G2 only modify MaxToolArgumentBytes (keeps tool <= 2097152)
			// G3, G4 only modify MaxToolArgumentDeltas
			toolVals := []int64{140000, 150000}
			toolCandidates := make(map[int64]bool, len(toolVals))
			for _, v := range toolVals {
				toolCandidates[v] = true
			}

			deltaVals := []int{5000, 6000}
			deltaCandidates := make(map[int]bool, len(deltaVals))
			for _, v := range deltaVals {
				deltaCandidates[v] = true
			}

			// Start the concurrent task that modifies tool bytes
			for i := 0; i < len(toolVals); i++ {
				wg.Add(1)
				val := toolVals[i]
				go func(tb int64) {
					defer wg.Done()
					_, gerr := db.UpdateOutputGuardSettings(ctx, OutputGuardSettingsUpdate{
						MaxToolArgumentBytes: &tb,
					})
					if gerr != nil {
						guardErrCh <- gerr
					}
				}(val)
			}

			// Start the concurrent task that modifies tool deltas
			for i := 0; i < len(deltaVals); i++ {
				wg.Add(1)
				val := deltaVals[i]
				go func(d int) {
					defer wg.Done()
					_, gerr := db.UpdateOutputGuardSettings(ctx, OutputGuardSettingsUpdate{
						MaxToolArgumentDeltas: &d,
					})
					if gerr != nil {
						guardErrCh <- gerr
					}
				}(val)
			}

			// Concurrently update the fully independent response_cache_settings group
			rcVals := []int64{10485760, 12582912, 14680064, 16777216}
			rcCandidates := make(map[int64]bool, len(rcVals))
			for _, v := range rcVals {
				rcCandidates[v] = true
			}
			for j := 0; j < len(rcVals); j++ {
				wg.Add(1)
				val := rcVals[j]
				go func(eb int64) {
					defer wg.Done()
					_, rcerr := db.UpdateResponseCacheSettings(ctx, ResponseCacheSettingsUpdate{
						LocalMaxEntryBytes: &eb,
					})
					if rcerr != nil {
						rcErrCh <- rcerr
					}
				}(val)
			}

			wg.Wait()
			close(guardErrCh)
			close(rcErrCh)

			for err := range guardErrCh {
				t.Fatalf("guard concurrent update error: %v", err)
			}
			for err := range rcErrCh {
				t.Fatalf("response cache concurrent update error: %v", err)
			}

			finalSettings, err := db.GetOutputGuardSettings(ctx)
			if err != nil {
				t.Fatalf("GetOutputGuardSettings after concurrent updates: %v", err)
			}

			// Check 1: Enabled stays unchanged (true)
			if finalSettings.Enabled == nil || *finalSettings.Enabled != true {
				t.Fatalf("expected Enabled=true preserved, got %+v", finalSettings)
			}
			// Check 2: the final value of concurrent writes to the same field must fall within the candidate value set
			if !toolCandidates[finalSettings.MaxToolArgumentBytes] {
				t.Fatalf("final MaxToolArgumentBytes %d not in candidate set %v", finalSettings.MaxToolArgumentBytes, toolCandidates)
			}
			if !deltaCandidates[finalSettings.MaxToolArgumentDeltas] {
				t.Fatalf("final MaxToolArgumentDeltas %d not in candidate set %v", finalSettings.MaxToolArgumentDeltas, deltaCandidates)
			}
			// Check 3: independent fields not modified concurrently keep their original values
			if finalSettings.MaxChannelBytes != afterInvalid.MaxChannelBytes || finalSettings.MaxRequestBytes != afterInvalid.MaxRequestBytes {
				t.Fatalf("independent fields corrupted! channel=%d request=%d", finalSettings.MaxChannelBytes, finalSettings.MaxRequestBytes)
			}
			// Check 4: revision (ConfigGeneration) real increment: every guard update changes the actual value, so the revision increment equals guardGoroutines exactly
			if finalSettings.ConfigGeneration != genBeforeConcurrent+int64(guardGoroutines) {
				t.Fatalf("expected generation increment of exactly %d (from %d to %d), got %d",
					guardGoroutines, genBeforeConcurrent, genBeforeConcurrent+int64(guardGoroutines), finalSettings.ConfigGeneration)
			}

			// Check 5: verify the final value of concurrent writes to the independent response_cache_settings
			finalRCSettings, err := db.GetResponseCacheSettings(ctx)
			if err != nil {
				t.Fatalf("GetResponseCacheSettings after concurrent updates: %v", err)
			}
			if !rcCandidates[finalRCSettings.LocalMaxEntryBytes] {
				t.Fatalf("final ResponseCache LocalMaxEntryBytes %d not in candidate set %v", finalRCSettings.LocalMaxEntryBytes, rcCandidates)
			}

			// 5. Scenario 21 gap 3: after ConfigGeneration MaxInt64 overflow rolls back, a second Get is strictly DeepEqual to before; the transaction exits normally with no deadlock
			if _, err := db.conn.ExecContext(ctx, `
				UPDATE system_settings SET output_guard_config_generation = $1 WHERE id = 1
			`, int64(math.MaxInt64)); err != nil {
				t.Fatalf("seed max generation: %v", err)
			}
			beforeOverflow, err := db.GetOutputGuardSettings(ctx)
			if err != nil {
				t.Fatalf("get settings before overflow: %v", err)
			}
			attemptToolBytes := int64(262144)
			_, err = db.UpdateOutputGuardSettings(ctx, OutputGuardSettingsUpdate{MaxToolArgumentBytes: &attemptToolBytes})
			if !errors.Is(err, ErrOutputGuardGenerationOverflow) {
				t.Fatalf("expected ErrOutputGuardGenerationOverflow, got %v", err)
			}
			afterOverflow, err := db.GetOutputGuardSettings(ctx)
			if err != nil {
				t.Fatalf("get settings after overflow (deadlock/tx check): %v", err)
			}
			if !reflect.DeepEqual(beforeOverflow, afterOverflow) {
				t.Fatalf("settings mutated after overflow error! before=%+v, after=%+v", beforeOverflow, afterOverflow)
			}

			// 6. Restore all 5 default fields and verify
			// Reset generation to the valid value 100
			if _, err := db.conn.ExecContext(ctx, `UPDATE system_settings SET output_guard_config_generation = 100 WHERE id = 1`); err != nil {
				t.Fatalf("reset generation failed: %v", err)
			}
			def := DefaultOutputGuardSettings()
			restoredAll, err := db.UpdateOutputGuardSettings(ctx, OutputGuardSettingsUpdate{
				Enabled:               def.Enabled,
				MaxToolArgumentBytes:  &def.MaxToolArgumentBytes,
				MaxToolArgumentDeltas: &def.MaxToolArgumentDeltas,
				MaxChannelBytes:       &def.MaxChannelBytes,
				MaxRequestBytes:       &def.MaxRequestBytes,
			})
			if err != nil {
				t.Fatalf("restore all defaults failed: %v", err)
			}
			if restoredAll.Enabled == nil || *restoredAll.Enabled != false ||
				restoredAll.MaxToolArgumentBytes != 262144 || restoredAll.MaxToolArgumentDeltas != 4096 ||
				restoredAll.MaxChannelBytes != 2097152 || restoredAll.MaxRequestBytes != 4194304 {
				t.Fatalf("restored defaults mismatch: %+v", restoredAll)
			}
			if restoredAll.ConfigGeneration != 101 {
				t.Fatalf("restored generation want 101, got %d", restoredAll.ConfigGeneration)
			}
		})
	}
}
