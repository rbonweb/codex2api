// security/outputguard/acceptance_detection_test.go
package outputguard

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"testing"
)

// Scenario 1: short-period repetition in the incident style (p<=256, triggers at 8192~10240, without waiting for 256 KiB)
func TestAcceptanceScenario1_AccidentPatternEarlyTrigger(t *testing.T) {
	rg := NewRequestGuard(Config{
		Enabled:               true,
		MaxToolArgumentBytes:  262144,
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       2097152,
		MaxRequestBytes:       4194304,
	})

	pattern := []byte("_auditfree.md_pluginless.tmp.md") // 31-byte period
	chunkSize := 128
	totalPushed := 0
	trippedAt := 0

	for totalPushed < 262144 {
		chunk := make([]byte, chunkSize)
		for i := 0; i < chunkSize; i++ {
			chunk[i] = pattern[(totalPushed+i)%len(pattern)]
		}
		err := rg.ObserveAppend("call_write", "", KindToolArguments, chunk)
		totalPushed += chunkSize
		if err != nil {
			trippedAt = totalPushed
			if err.Error() != "repetition" {
				t.Fatalf("expected repetition error, got %v", err)
			}
			break
		}
	}

	if trippedAt == 0 {
		t.Fatalf("expected guard to trip, but reached totalPushed=%d", totalPushed)
	}
	if trippedAt < 8192 {
		t.Fatalf("expected early trigger near 8192-10240 bytes, but triggered too early at %d bytes", trippedAt)
	}
	if trippedAt > 10240 {
		t.Fatalf("expected early trigger near 8192-10240 bytes, but triggered at %d bytes", trippedAt)
	}
	if !rg.IsTripped() || rg.TrippedReason() != "repetition" {
		t.Fatalf("expected tripped with repetition, got %s", rg.TrippedReason())
	}
}

// Scenario 2: the same semantic stream is detected whether split into 1-byte, small/large deltas, or random boundaries; splitting must not make the repetition disappear
func TestAcceptanceScenario2_ChunkInvariance(t *testing.T) {
	pattern := []byte("defensive_pipeline_token_replay_") // 32 bytes
	var fullStream []byte
	for len(fullStream) < 12000 {
		fullStream = append(fullStream, pattern...)
	}

	chunkSizes := []int{1, 7, 64, 512, 1024, 2048}
	for _, size := range chunkSizes {
		t.Run(fmt.Sprintf("FixedChunkSize_%d", size), func(t *testing.T) {
			rg := NewRequestGuard(Config{
				Enabled:               true,
				MaxToolArgumentBytes:  262144,
				MaxToolArgumentDeltas: 65536, // relax the delta count so 1-byte splitting is not masked early at 4096 deltas
				MaxChannelBytes:       2097152,
				MaxRequestBytes:       4194304,
			})
			hit := false
			for offset := 0; offset < len(fullStream); offset += size {
				end := offset + size
				if end > len(fullStream) {
					end = len(fullStream)
				}
				err := rg.ObserveAppend("c_inv", "", KindToolArguments, fullStream[offset:end])
				if err != nil {
					if err.Error() == "repetition" {
						hit = true
						break
					}
					t.Fatalf("unexpected error: %v", err)
				}
			}
			if !hit {
				t.Fatalf("chunk size %d failed to detect repetition across boundaries", size)
			}
		})
	}

	t.Run("RandomChunkBoundaries", func(t *testing.T) {
		r := rand.New(rand.NewSource(42))
		rg := NewRequestGuard(Config{
			Enabled:               true,
			MaxToolArgumentBytes:  262144,
			MaxToolArgumentDeltas: 65536,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
		})
		offset := 0
		hit := false
		for offset < len(fullStream) {
			sz := r.Intn(256) + 1
			end := offset + sz
			if end > len(fullStream) {
				end = len(fullStream)
			}
			err := rg.ObserveAppend("c_rand", "", KindToolArguments, fullStream[offset:end])
			if err != nil {
				if err.Error() == "repetition" {
					hit = true
					break
				}
				t.Fatalf("unexpected error: %v", err)
			}
			offset = end
		}
		if !hit {
			t.Fatalf("random chunk boundaries failed to detect repetition")
		}
	})
}

// Scenario 3: a single large delta with an exact 9215-byte short-period fragment embedded in the middle still triggers when the tail is normal
func TestAcceptanceScenario3_Embedded9215BytesAnd8192Boundary(t *testing.T) {
	// Build the exact 9215-byte periodic fragment: repeat, ceil, then slice to exactly 9215 bytes, and verify several different starting points
	unit := []byte("1234567890abcdefghijklmno") // p=25
	repeats := int(math.Ceil(float64(9215) / float64(len(unit))))

	for _, startOffset := range []int{0, 7, 13, 24} {
		shiftedUnit := append(unit[startOffset:], unit[:startOffset]...)
		shiftedPeriodic := bytes.Repeat(shiftedUnit, repeats)[:9215]
		if len(shiftedPeriodic) != 9215 {
			t.Fatalf("expected 9215 bytes, got %d", len(shiftedPeriodic))
		}

		prng := rand.New(rand.NewSource(int64(1001 + startOffset)))
		prefix := make([]byte, 500)
		prng.Read(prefix)
		suffix := make([]byte, 1000)
		prng.Read(suffix)

		var singleLargeDelta []byte
		singleLargeDelta = append(singleLargeDelta, prefix...)
		singleLargeDelta = append(singleLargeDelta, shiftedPeriodic...)
		singleLargeDelta = append(singleLargeDelta, suffix...)

		rgLoop := NewRequestGuard(Config{
			Enabled:               true,
			MaxToolArgumentBytes:  262144,
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
		})
		err := rgLoop.ObserveAppend("c_embed", "", KindText, singleLargeDelta)
		if err == nil || err.Error() != "repetition" {
			t.Fatalf("offset %d: expected repetition error on 9215 bytes segment, got %v", startOffset, err)
		}
	}

	t.Run("Aligned8192BytesHits", func(t *testing.T) {
		rgAligned := NewRequestGuard(Config{
			Enabled:         true,
			MaxChannelBytes: 2097152,
			MaxRequestBytes: 4194304,
		})
		alignedData := bytes.Repeat([]byte("abcdef0123456789"), 512) // 8192 bytes, p=16
		err := rgAligned.ObserveAppend("c_aligned", "", KindText, alignedData)
		if err == nil || err.Error() != "repetition" {
			t.Fatalf("expected repetition error on 8192-aligned window, got %v", err)
		}
	})
}

// Scenario 4: interleaved text / thinking / tool streams do not contaminate each other; a real item_id with a late call_id gets an alias
func TestAcceptanceScenario4_InterleavedChannelsAndNoReset(t *testing.T) {
	rg := NewRequestGuard(Config{
		Enabled:               true,
		MaxToolArgumentBytes:  262144,
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       2097152,
		MaxRequestBytes:       4194304,
	})

	textData := []byte("Normal text message token. ")
	thinkingData := []byte("Deep reflection thoughts. ")
	toolData := []byte(`{"arg":"value_`)

	for i := 0; i < 50; i++ {
		if err := rg.ObserveAppend("c_text", "", KindText, textData); err != nil {
			t.Fatalf("text append failed: %v", err)
		}
		if err := rg.ObserveAppend("c_think", "", KindThinking, thinkingData); err != nil {
			t.Fatalf("thinking append failed: %v", err)
		}
		if err := rg.ObserveAppend("item_tool_1", "", KindToolArguments, toolData); err != nil {
			t.Fatalf("tool append failed: %v", err)
		}
	}

	// The late call_id is bound to item_tool_1 as an alias
	if err := rg.ObserveAppend("call_999", "item_tool_1", KindToolArguments, []byte(`1"}`)); err != nil {
		t.Fatalf("alias append failed: %v", err)
	}

	cg := rg.channels["item_tool_1"]
	if cg == nil {
		cg = rg.channels["call_999"]
	}
	if cg == nil {
		t.Fatalf("channel not resolved")
	}
	if cg.DeltaCount() != 51 {
		t.Fatalf("delta count was reset! want 51, got %d", cg.DeltaCount())
	}
	expectedHighwater := int64(50*len(toolData) + len(`1"}`))
	if cg.Highwater() != expectedHighwater {
		t.Fatalf("highwater was reset or inaccurate! want %d, got %d", expectedHighwater, cg.Highwater())
	}

	// Verify the index alias mapping: appending with a numeric index form (such as "call_index_0") to a mapped channel must point to the same channel instance and accumulate the high-water mark
	if err := rg.ObserveAppend("call_index_0", "call_999", KindToolArguments, []byte(`,"extra":2}`)); err != nil {
		t.Fatalf("index alias append failed: %v", err)
	}
	if cg.DeltaCount() != 52 {
		t.Fatalf("index alias delta count want 52, got %d", cg.DeltaCount())
	}
	expectedHighwater += int64(len(`,"extra":2}`))
	if cg.Highwater() != expectedHighwater {
		t.Fatalf("highwater after index alias want %d, got %d", expectedHighwater, cg.Highwater())
	}
}

// Scenario 5: exact hard-budget blocking is kept separate from ErrScanBudgetExhausted
func TestAcceptanceScenario5_BudgetInterception(t *testing.T) {
	// 5a: tool bytes blocked at exactly +1
	t.Run("ToolBytesBudgetExactPlusOne", func(t *testing.T) {
		rg := NewRequestGuard(Config{
			Enabled:               true,
			MaxToolArgumentBytes:  262144,
			MaxToolArgumentDeltas: 65536,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
		})
		prng := rand.New(rand.NewSource(2001))
		exactData := make([]byte, 262144)
		prng.Read(exactData)
		if err := rg.ObserveAppend("c_exact", "", KindToolArguments, exactData); err != nil {
			t.Fatalf("exact tool bytes should pass, got: %v", err)
		}
		err := rg.ObserveAppend("c_exact", "", KindToolArguments, []byte("x"))
		if err == nil || err.Error() != "tool_argument_bytes" {
			t.Fatalf("expected tool_argument_bytes error, got %v", err)
		}
	})

	// 5b: fine-grained arguments exceed the delta limit (4096 deltas)
	t.Run("ToolDeltasBudget", func(t *testing.T) {
		rg := NewRequestGuard(Config{
			Enabled:               true,
			MaxToolArgumentBytes:  262144,
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
		})
		for i := 0; i < 4096; i++ {
			if err := rg.ObserveAppend("c_tool", "", KindToolArguments, []byte("x")); err != nil {
				t.Fatalf("unexpected err at %d: %v", i, err)
			}
		}
		err := rg.ObserveAppend("c_tool", "", KindToolArguments, []byte("x"))
		if err == nil || err.Error() != "tool_argument_deltas" {
			t.Fatalf("expected tool_argument_deltas error, got %v", err)
		}
	})

	// 5c: crossing channels to get around the per-item limit -> blocked as request_bytes
	t.Run("RequestBytesBudget", func(t *testing.T) {
		rg := NewRequestGuard(Config{
			Enabled:               true,
			MaxToolArgumentBytes:  262144,
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
		})
		prng := rand.New(rand.NewSource(2002))
		buf1 := make([]byte, 2097152)
		prng.Read(buf1)
		buf2 := make([]byte, 2097152)
		prng.Read(buf2)

		if err := rg.ObserveAppend("c1", "", KindText, buf1); err != nil {
			t.Fatalf("c1 failed: %v", err)
		}
		if err := rg.ObserveAppend("c2", "", KindText, buf2); err != nil {
			t.Fatalf("c2 failed: %v", err)
		}
		err := rg.ObserveAppend("c3", "", KindText, []byte("a"))
		if err == nil || err.Error() != "request_bytes" {
			t.Fatalf("expected request_bytes error, got %v", err)
		}
	})

	// 5d: flood of empty deltas and 131072 business events
	t.Run("EmptyDeltaEventFlood", func(t *testing.T) {
		rg := NewRequestGuard(Config{
			Enabled:               true,
			MaxToolArgumentBytes:  262144,
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
		})
		for i := 0; i < MaxRequestEvents; i++ {
			if err := rg.ObserveAppend("c_empty", "", KindText, []byte{}); err != nil {
				t.Fatalf("empty event %d rejected: %v", i, err)
			}
		}
		err := rg.ObserveAppend("c_empty", "", KindText, []byte{})
		if err == nil || err.Error() != "request_events" {
			t.Fatalf("expected request_events error, got %v", err)
		}
	})

	// 5e: flood of 128 channels
	t.Run("ChannelCountFlood", func(t *testing.T) {
		rg := NewRequestGuard(Config{
			Enabled:               true,
			MaxToolArgumentBytes:  262144,
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
		})
		for i := 0; i < MaxRequestChannels; i++ {
			cid := fmt.Sprintf("chan_%d", i)
			if err := rg.ObserveAppend(cid, "", KindText, []byte("ok")); err != nil {
				t.Fatalf("channel %d rejected: %v", i, err)
			}
		}
		err := rg.ObserveAppend("chan_129", "", KindText, []byte("fail"))
		if err == nil || err.Error() != "channel_count" {
			t.Fatalf("expected channel_count error, got %v", err)
		}
	})

	// 5f: lower-layer ErrScanBudgetExhausted is mapped in isolation (internal sentinel errors must never leak; they map to a public reason)
	t.Run("ScanBudgetExhaustionMapping", func(t *testing.T) {
		// ToolArguments channel exhausts the scan budget -> mapped to tool_argument_bytes
		rgTool := NewRequestGuard(Config{
			Enabled:               true,
			MaxToolArgumentBytes:  262144, // scan budget = 524288
			MaxToolArgumentDeltas: 65536,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
		})
		chunk := make([]byte, 1024)
		rand.New(rand.NewSource(501)).Read(chunk)
		for i := 0; i < 256; i++ {
			if err := rgTool.ObserveAppend("tool_map", "", KindToolArguments, chunk); err != nil {
				t.Fatalf("append %d failed: %v", i, err)
			}
		}
		// The terminal snapshot establishes the baseline
		full := bytes.Repeat(chunk, 256)
		if err := rgTool.ObserveSnapshot("tool_map", "", KindToolArguments, full, "arguments.done"); err != nil {
			t.Fatalf("snapshot baseline failed: %v", err)
		}
		// Variant snapshot exceeds the scan budget -> must map to tool_argument_bytes, never leak the internal ErrScanBudgetExhausted
		variant := bytes.Clone(full)
		variant[100] ^= 0xEE
		err := rgTool.ObserveSnapshot("tool_map", "", KindToolArguments, variant, "item.done")
		if err == nil || err.Error() != "tool_argument_bytes" {
			t.Fatalf("expected mapped reason tool_argument_bytes, got %v", err)
		}

		// Text channel exhausts the scan budget -> mapped to channel_bytes
		rgText := NewRequestGuard(Config{
			Enabled:               true,
			MaxToolArgumentBytes:  262144,
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       131072, // scan budget = 262144
			MaxRequestBytes:       4194304,
		})
		for i := 0; i < 128; i++ {
			if err := rgText.ObserveAppend("text_map", "", KindText, chunk); err != nil {
				t.Fatalf("text append %d failed: %v", i, err)
			}
		}
		textFull := bytes.Repeat(chunk, 128)
		textVariant := bytes.Clone(textFull)
		textVariant[100] ^= 0xEE
		if err := rgText.ObserveSnapshot("text_map", "", KindText, textFull, "arguments.done"); err != nil {
			t.Fatalf("text snapshot failed: %v", err)
		}
		errText := rgText.ObserveSnapshot("text_map", "", KindText, textVariant, "item.done")
		if errText == nil || errText.Error() != "channel_bytes" {
			t.Fatalf("expected mapped reason channel_bytes, got %v", errText)
		}
	})
}

// Scenario 6: boundary values, UTF-8 Chinese text, emoji, escape characters and int64 overflow protection
func TestAcceptanceScenario6_BoundaryValuesAndUTF8(t *testing.T) {
	rg := NewRequestGuard(Config{
		Enabled:               true,
		MaxToolArgumentBytes:  262144,
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       2097152,
		MaxRequestBytes:       4194304,
	})

	multilingual := []byte("这是一段中文测试，包含特殊符号与表情：👨‍👩‍👧‍👦 🎉 🚀 以及转义符：\\n \\t \\\" \\\\ 还有非闭合参数: {\"file\": \"/path/to/file.txt\"")
	if err := rg.ObserveAppend("c_multi", "", KindText, multilingual); err != nil {
		t.Fatalf("multilingual append failed: %v", err)
	}

	// int64 overflow protection: controlled mutation as counter-evidence
	t.Run("Int64OverflowGuard", func(t *testing.T) {
		rgOverflow := NewRequestGuard(Config{
			Enabled:               true,
			MaxToolArgumentBytes:  262144,
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
		})
		_ = rgOverflow.ObserveAppend("c_overflow", "", KindText, []byte("init"))

		// Simulate a high-water mark near MaxInt64; assert that addition overflow protection blocks directly and returns an error
		cg := rgOverflow.channels["c_overflow"]
		cg.highwater = math.MaxInt64 - 10
		rgOverflow.requestBytes = math.MaxInt64 - 10

		err := rgOverflow.ObserveAppend("c_overflow", "", KindText, make([]byte, 20))
		if err == nil {
			t.Fatalf("expected overflow rejection, but got nil (highwater/requestBytes may have wrapped around!)")
		}
	})
}

// Scenario 7: full snapshots cannot escape (done-only, terminal-only, native object, changing snapshot)
func TestAcceptanceScenario7_SnapshotsCannotBypass(t *testing.T) {
	// 7a: basic 9215B periodic snapshot blocked (strictly repeat, ceil, then slice to exactly 9215 bytes)
	t.Run("BasicPeriodicSnapshot", func(t *testing.T) {
		rg := NewRequestGuard(Config{
			Enabled:               true,
			MaxToolArgumentBytes:  262144,
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
		})
		unit := []byte("snapshot_period_pattern_test1234") // p=32
		repeats := int(math.Ceil(float64(9215) / float64(len(unit))))
		periodicSnapshot := bytes.Repeat(unit, repeats)[:9215]
		err := rg.ObserveSnapshot("c_snap", "", KindText, periodicSnapshot, "response.completed")
		if err == nil || err.Error() != "repetition" {
			t.Fatalf("snapshot with repetition must be rejected, got: %v", err)
		}
	})

	// 7b: done-only snapshot (no preceding append; periodic arguments.done / output_item.done snapshot sent directly)
	t.Run("DoneOnlySnapshot", func(t *testing.T) {
		rg := NewRequestGuard(Config{
			Enabled:               true,
			MaxToolArgumentBytes:  262144,
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
		})
		unit := []byte("tool_arg_done_only_pattern_abc!") // 31 bytes
		repeats := int(math.Ceil(float64(9215) / float64(len(unit))))
		snap := bytes.Repeat(unit, repeats)[:9215]
		err := rg.ObserveSnapshot("tool_done_only", "", KindToolArguments, snap, "arguments.done")
		if err == nil || err.Error() != "repetition" {
			t.Fatalf("done-only snapshot must be rejected, got: %v", err)
		}
	})

	// 7c: terminal-only terminal snapshot blocked (only the terminal stage marker is sent)
	t.Run("TerminalOnlySnapshot", func(t *testing.T) {
		rg := NewRequestGuard(Config{
			Enabled:               true,
			MaxToolArgumentBytes:  262144,
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
		})
		unit := []byte("terminal_only_stream_finished__!") // 32 bytes
		repeats := int(math.Ceil(float64(9215) / float64(len(unit))))
		snap := bytes.Repeat(unit, repeats)[:9215]
		err := rg.ObserveSnapshot("terminal_only", "", KindText, snap, "item.done")
		if err == nil || err.Error() != "repetition" {
			t.Fatalf("terminal-only snapshot must be rejected, got: %v", err)
		}
	})

	// 7d: native object compact-serialized snapshot cannot escape (simulates native object serialized snapshots in protocols such as Gemini)
	t.Run("NativeObjectSnapshot", func(t *testing.T) {
		rg := NewRequestGuard(Config{
			Enabled:               true,
			MaxToolArgumentBytes:  262144,
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
		})
		unit := []byte(`{"key":"cyclic_val_12345678"},`) // 30 bytes
		repeats := int(math.Ceil(float64(9215) / float64(len(unit))))
		snap := bytes.Repeat(unit, repeats)[:9215]
		err := rg.ObserveSnapshot("native_obj", "", KindToolArguments, snap, "response.completed")
		if err == nil || err.Error() != "repetition" {
			t.Fatalf("native object snapshot must be rejected, got: %v", err)
		}
	})

	// 7e: changing snapshot (content differs between the previous and the next snapshot; the delta and the changed content are fully verified)
	t.Run("MutatingSnapshot", func(t *testing.T) {
		rg := NewRequestGuard(Config{
			Enabled:               true,
			MaxToolArgumentBytes:  262144,
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
		})
		// First legal snapshot
		initialSnap := []byte("initial valid snapshot content without any repetition")
		if err := rg.ObserveSnapshot("c_mut", "", KindText, initialSnap, ""); err != nil {
			t.Fatalf("initial snapshot failed: %v", err)
		}
		// The second snapshot mutates into a 9215B periodic snapshot
		unit := []byte("mutating_snapshot_pattern_1234!") // 31 bytes
		repeats := int(math.Ceil(float64(9215) / float64(len(unit))))
		mutatedSnap := bytes.Repeat(unit, repeats)[:9215]
		err := rg.ObserveSnapshot("c_mut", "", KindText, mutatedSnap, "response.completed")
		if err == nil || err.Error() != "repetition" {
			t.Fatalf("mutating snapshot into repetition must be rejected, got: %v", err)
		}
	})
}

// Scenario 8: observable counts for the 8a/8b/8c hard cost upper bounds
func TestAcceptanceScenario8_BudgetScanPermitsAndBoundingCases(t *testing.T) {
	// Case 8a: scan budget exhaustion and rejection of excess variants
	t.Run("Case8a", func(t *testing.T) {
		toolLimit := int64(262144) // 256 KiB -> scan budget 512 KiB
		cg := NewChannelGuard("t8a", KindToolArguments, toolLimit, 4096)

		prng := rand.New(rand.NewSource(801))
		chunk := make([]byte, 1024)
		for i := 0; i < 256; i++ {
			prng.Read(chunk)
			g, c, err := cg.CheckAppend(chunk)
			if err != nil {
				t.Fatalf("append %d failed: %v", i, err)
			}
			_ = cg.CommitAppend(chunk, g, c)
		}
		if cg.RemainingScanBudget() != 262144 {
			t.Fatalf("want 262144, got %d", cg.RemainingScanBudget())
		}

		snap := make([]byte, 262144)
		prng.Read(snap)
		g, c, skip, err := cg.CheckSnapshot(snap, "arguments.done")
		if err != nil || skip {
			t.Fatalf("first snapshot failed: err=%v, skip=%v", err, skip)
		}
		_ = cg.CommitSnapshot(snap, g, c, "arguments.done")

		if cg.RemainingScanBudget() != 0 {
			t.Fatalf("want 0 scan budget, got %d", cg.RemainingScanBudget())
		}

		variant := make([]byte, 262144)
		copy(variant, snap)
		variant[500] ^= 0xFF

		_, _, _, err = cg.CheckSnapshot(variant, "item.done")
		if !errors.Is(err, ErrScanBudgetExhausted) {
			t.Fatalf("want ErrScanBudgetExhausted, got %v", err)
		}
	})

	// Case 8b: the terminal chain has 3 permits (at most 3 cost=0 full-text comparisons) and blocks beyond 3; real internal private statistics assert (scan <= 2*limit, total verification <= 5*limit)
	t.Run("Case8b", func(t *testing.T) {
		toolLimit := int64(262144) // limit = 256 KiB
		cg := NewChannelGuard("t8b", KindToolArguments, toolLimit, 4096)

		snap := make([]byte, 262144)
		rand.New(rand.NewSource(802)).Read(snap)

		cg.remainingScanBudget = 262144 // reserve a 256 KiB scan budget (the preceding append already consumed 256 KiB)
		cg.highwater = 262144
		g, c, skip, err := cg.CheckSnapshot(snap, "arguments.done")
		if err != nil || skip || c != 262144 {
			t.Fatalf("arguments.done failed: err=%v, skip=%v, cost=%d", err, skip, c)
		}
		_ = cg.CommitSnapshot(snap, g, c, "arguments.done")

		// Real internal private statistics assertion: verify that the SHA-256 baseline digest is established and the scan budget is deducted
		if !cg.hasBaselineHash || cg.terminalBaselineHash != sha256.Sum256(snap) {
			t.Fatalf("baseline sha256 digest not established")
		}
		if cg.remainingScanBudget != 0 {
			t.Fatalf("scan budget must be exhausted (0), got %d", cg.remainingScanBudget)
		}
		// arguments.done already counted the 1st terminal allowlist consumption when it established the baseline
		if cg.terminalPermitsUsed != 1 {
			t.Fatalf("arguments.done terminalPermitsUsed want 1, got %d", cg.terminalPermitsUsed)
		}

		// Permit 1: item.done (cost=0)
		_, c2, skip2, err2 := cg.CheckSnapshot(snap, "item.done")
		if err2 != nil || !skip2 || c2 != 0 {
			t.Fatalf("item.done permit failed: err=%v, skip=%v, cost=%d", err2, skip2, c2)
		}
		_ = cg.CommitSnapshot(snap, 0, c2, "item.done")
		if cg.terminalPermitsUsed != 2 {
			t.Fatalf("terminalPermitsUsed want 2, got %d", cg.terminalPermitsUsed)
		}

		// Permit 2: response.completed (cost=0)
		_, c3, skip3, err3 := cg.CheckSnapshot(snap, "response.completed")
		if err3 != nil || !skip3 || c3 != 0 {
			t.Fatalf("response.completed permit failed: err=%v, skip=%v, cost=%d", err3, skip3, c3)
		}
		_ = cg.CommitSnapshot(snap, 0, c3, "response.completed")
		if cg.terminalPermitsUsed != 3 {
			t.Fatalf("terminalPermitsUsed want 3, got %d", cg.terminalPermitsUsed)
		}

		// Strictly verify the total processing upper bound: the total scan budget is 2*limit (512 KiB); at most 3 extra full-text hash comparisons among the 3 terminal comparisons (3*limit = 768 KiB); overall maximum data processing and verification is strictly bounded by 5*limit (1280 KiB)
		const maxAllowedScanned = 2 * 262144
		const maxAllowedVerified = 5 * 262144
		scannedTotal := (2 * toolLimit) - cg.remainingScanBudget
		if scannedTotal > maxAllowedScanned {
			t.Fatalf("total scanned bytes %d exceeds 2*limit budget %d", scannedTotal, maxAllowedScanned)
		}
		totalVerifiedBytes := scannedTotal + int64(cg.terminalPermitsUsed)*toolLimit
		if totalVerifiedBytes > maxAllowedVerified {
			t.Fatalf("total verified bytes %d exceeds 5*limit bound %d", totalVerifiedBytes, maxAllowedVerified)
		}

		// A repeated terminal frame of the same type is not eligible; the full cost applies and it exceeds the budget
		_, _, _, err4 := cg.CheckSnapshot(snap, "response.completed")
		if !errors.Is(err4, ErrScanBudgetExhausted) {
			t.Fatalf("duplicate terminal frame want ErrScanBudgetExhausted, got %v", err4)
		}
	})

	// Case 8c: a short total length (<8192) is not misjudged
	t.Run("Case8c_ShortSnapshotNoFalsePositive", func(t *testing.T) {
		cg := NewChannelGuard("t8c", KindText, 2097152, 4096)
		shortPeriodic := bytes.Repeat([]byte("repeat_me_1234567890"), 100) // 2000 bytes
		g, c, _, err := cg.CheckSnapshot(shortPeriodic, "")
		if err != nil {
			t.Fatalf("short periodic snapshot check error: %v", err)
		}
		if err := cg.CommitSnapshot(shortPeriodic, g, c, ""); err != nil {
			t.Fatalf("short periodic snapshot <8192 must not trip repetition, got %v", err)
		}
	})
}

// Scenario 9: 7 kinds of normal samples never produce a false positive
func TestAcceptanceScenario9_NormalWorkloadsDoNotFalsePositive(t *testing.T) {
	testCases := []struct {
		name string
		data []byte
	}{
		{
			name: "NormalCode",
			data: []byte(`package main
import "fmt"
func processItems(items []string) error {
	for i, item := range items {
		if err := handleItem(i, item); err != nil {
			return fmt.Errorf("failed at %d: %w", i, err)
		}
	}
	return nil
}`),
		},
		{
			name: "NormalMarkdown",
			data: []byte(`# Architecture Overview
## Section 1: Introduction
This document describes the pipeline for streaming events.
- Item 1: High performance buffer
- Item 2: Zero allocation scanner
| Header A | Header B |
|---|---|
| Value 1 | Value 2 |`),
		},
		{
			name: "MultiToolCalls",
			data: []byte(`{"call_id": "call_1", "name": "read_file", "path": "main.go"}`),
		},
		{
			name: "UnclosedJSON",
			data: []byte(`{"message": "Hello world, here is some nested unclosed arguments: {\"key\": \"value`),
		},
		{
			name: "ChineseAndEnglish",
			data: []byte(`这是一个中英文混合的文档。The quick brown fox jumps over the lazy dog. 包含各种日常标点符号。`),
		},
		{
			name: "ImageBase64Field",
			data: []byte(`{"image": "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="}`),
		},
		{
			name: "HeartbeatAndComments",
			data: []byte(": ping - keepalive heartbeat\n: sse-comment-data\n\n"),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			rg := NewRequestGuard(Config{
				Enabled:               true,
				MaxToolArgumentBytes:  262144,
				MaxToolArgumentDeltas: 4096,
				MaxChannelBytes:       2097152,
				MaxRequestBytes:       4194304,
			})
			if err := rg.ObserveAppend("c_norm", "", KindText, tc.data); err != nil {
				t.Fatalf("normal workload %s falsely tripped: %v", tc.name, err)
			}
		})
	}
}

// Scenario 10: deliberately large periodic data is blocked by the rules (a deliberate safety trade-off; zero false positives are not claimed)
func TestAcceptanceScenario10_IntentionalRepetitionBlockedAsDesigned(t *testing.T) {
	rg := NewRequestGuard(Config{
		Enabled:               true,
		MaxToolArgumentBytes:  262144,
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       2097152,
		MaxRequestBytes:       4194304,
	})

	intentional := bytes.Repeat([]byte("test_test_test_"), 800) // 12000 bytes
	err := rg.ObserveAppend("c_intent", "", KindText, intentional)
	if err == nil || err.Error() != "repetition" {
		t.Fatalf("intentional large periodic output must be blocked, got: %v", err)
	}
}

// Scenario 11: tail-check rule assertions (a short stream under 8192 passes; a full 8192 window not aligned to the 1024 tail check)
func TestAcceptanceScenario11_TailCheckRulesVerification(t *testing.T) {
	// 11a: a short normal stream (<8192 bytes) never produces a false positive when end is received
	t.Run("ShortStreamTailCheckAllowed", func(t *testing.T) {
		rg := NewRequestGuard(Config{
			Enabled:               true,
			MaxToolArgumentBytes:  262144,
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
		})
		shortData := []byte(strings.Repeat("Short repetitive greeting! ", 20)) // ~540 bytes
		if err := rg.ObserveAppend("c_short", "", KindText, shortData); err != nil {
			t.Fatalf("append failed: %v", err)
		}
		if err := rg.EndChannel("c_short"); err != nil {
			t.Fatalf("EndChannel tail check must not trip for short stream <8192 bytes, got: %v", err)
		}
	})

	// 11b: immediate tail check for a full 8192 window whose tail is not aligned to 1024 bytes
	t.Run("Full8192UnalignedTailCheck", func(t *testing.T) {
		rg := NewRequestGuard(Config{
			Enabled:               true,
			MaxToolArgumentBytes:  262144,
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
		})
		prng := rand.New(rand.NewSource(1101))
		noise := make([]byte, 500)
		prng.Read(noise)
		_ = rg.ObserveAppend("c_tail", "", KindText, noise)

		periodData := bytes.Repeat([]byte("strict_tail_check_pattern_123456"), 8192/32) // 8192 bytes
		_ = rg.ObserveAppend("c_tail", "", KindText, periodData)

		err := rg.EndChannel("c_tail")
		if err == nil || err.Error() != "repetition" {
			t.Fatalf("EndChannel tail check must detect repetition on latest 8192 bytes window, got: %v", err)
		}
	})
}

// Additional regression counterexample: the Append priority regression assertion found in review (Spec §5.3 step 4 request_bytes comes before step 6 scan_budget)
// Note: the 01 core already fixed the addition overflow and the pipeline decision order in the targetfix stage; this test makes a strict assertion rather than only t.Log
func TestAppendScanBudgetVsRequestBytesPrecedenceRegression(t *testing.T) {
	limit := int64(1 << 20)       // channel 1 MiB -> scan budget 2 MiB
	reqLimit := int64(600 * 1024) // request ≈0.586 MiB
	rg := NewRequestGuard(Config{
		Enabled:               true,
		MaxToolArgumentBytes:  262144,
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       limit,
		MaxRequestBytes:       reqLimit,
	})

	half := make([]byte, 512*1024)
	rand.New(rand.NewSource(9101)).Read(half)
	if err := rg.ObserveAppend("c1", "", KindText, half); err != nil {
		t.Fatalf("seed append failed: %v", err)
	}

	// Three ineligible 0.5 MiB variant snapshots drive the scan budget down to 0
	variant := make([]byte, 512*1024)
	copy(variant, half)
	for i := 0; i < 3; i++ {
		variant[i*100] ^= 0xAA
		if err := rg.ObserveSnapshot("c1", "", KindText, variant, ""); err != nil {
			t.Fatalf("variant snapshot %d failed: %v", i, err)
		}
	}

	// Append another 0.2 MiB: this exceeds request_bytes and the scan budget is insufficient at the same time
	// The Spec §5.3 strict order requires step 4 before step 6; assert precisely that request_bytes is returned, not merely log it with t.Log
	probe := make([]byte, 200*1024)
	rand.New(rand.NewSource(9102)).Read(probe)
	err := rg.ObserveAppend("c1", "", KindText, probe)
	if err == nil {
		t.Fatalf("expected rejection, got nil")
	}
	if err.Error() != "request_bytes" {
		t.Fatalf("Spec §5.3 precedence violation: expected request_bytes, got %q", err.Error())
	}
}
