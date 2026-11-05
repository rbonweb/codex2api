package outputguard

import (
	"bytes"
	"fmt"
	"math"
	"math/rand"
	"sync"
	"testing"
)

func defaultTestConfig() Config {
	return Config{
		Enabled:               true,
		MaxToolArgumentBytes:  262144, // 256 KiB
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       2097152, // 2 MiB
		MaxRequestBytes:       4194304, // 4 MiB
	}
}

func TestRequestGuardRequestEventsFlood(t *testing.T) {
	rg := NewRequestGuard(defaultTestConfig())
	for i := 0; i < MaxRequestEvents; i++ {
		cid := fmt.Sprintf("c_%d", i%50)
		delta := []byte{byte(i % 250)}
		if err := rg.ObserveAppend(cid, "", KindText, delta); err != nil {
			t.Fatalf("unexpected error at event %d: %v", i, err)
		}
	}
	// The 131073rd event must be rejected and judged as request_events (Spec §5.3 step 1)
	err := rg.ObserveAppend("c_0", "", KindText, []byte("x"))
	if err == nil || err.Error() != "request_events" {
		t.Fatalf("expected request_events, got %v", err)
	}
	if !rg.IsTripped() || rg.TrippedReason() != "request_events" {
		t.Fatalf("expected tripped with request_events, got %s", rg.TrippedReason())
	}
}

func TestRequestGuardChannelFlood(t *testing.T) {
	rg := NewRequestGuard(defaultTestConfig())
	// Verify the cumulative 128-channel limit:
	for i := 0; i < MaxRequestChannels; i++ {
		cid := fmt.Sprintf("chan_%d", i)
		if err := rg.ObserveAppend(cid, "", KindText, []byte("x")); err != nil {
			t.Fatalf("channel %d error: %v", i, err)
		}
	}
	// The 129th channel must be rejected and judged as channel_count (Spec §5.3 step 2)
	err := rg.ObserveAppend("channel_129", "", KindText, []byte("x"))
	if err == nil || err.Error() != "channel_count" {
		t.Fatalf("expected channel_count, got %v", err)
	}
}

func TestRequestGuardChannelCumulativeNotReturnedOnEnd(t *testing.T) {
	rg := NewRequestGuard(defaultTestConfig())
	// Verify that EndChannel does not return the cumulative channel quota (Spec §5.1)
	for i := 0; i < MaxRequestChannels; i++ {
		cid := fmt.Sprintf("chan_%d", i)
		_ = rg.ObserveAppend(cid, "", KindText, []byte("x"))
		_ = rg.EndChannel(cid)
	}
	// Try to open another channel; it must be rejected as channel_count
	err := rg.ObserveAppend("extra_channel", "", KindText, []byte("x"))
	if err == nil || err.Error() != "channel_count" {
		t.Fatalf("expected channel_count rejection after 128 closed channels, got %v", err)
	}
}

func TestRequestGuardEndChannelExecutesTailCheck(t *testing.T) {
	rg := NewRequestGuard(defaultTestConfig())
	// Build a stream that triggers no checkpoint during Push but already contains an 8192-byte period when the stream ends
	pattern := []byte("0123456789")
	periodicBody := bytes.Repeat(pattern, 8192/10)
	periodicBody = append(periodicBody, pattern[:8192%10]...) // exactly an 8192B period
	// First push 100 bytes of stray data
	prefix := make([]byte, 100)
	for i := range prefix {
		prefix[i] = byte(i)
	}
	full := append(prefix, periodicBody...) // 8292 bytes, not yet at the next 1024 checkpoint

	// Push the data; no repetition is triggered during Push (the 8192 checkpoint includes the prefix stray data, and 8292 has not reached a checkpoint)
	for offset := 0; offset < len(full); offset += 500 {
		end := offset + 500
		if end > len(full) {
			end = len(full)
		}
		_ = rg.ObserveAppend("c_tail", "", KindText, full[offset:end])
	}
	if rg.IsTripped() {
		t.Fatalf("expected not tripped before EndChannel")
	}

	// Call EndChannel: it must run the tail check first, find the short periodic repetition, block it successfully, and freeze repetition!
	err := rg.EndChannel("c_tail")
	if err == nil || err.Error() != "repetition" {
		t.Fatalf("expected repetition hit during EndChannel tail check, got %v", err)
	}
	if !rg.IsTripped() || rg.TrippedReason() != "repetition" {
		t.Fatalf("expected tripped with repetition, got %s", rg.TrippedReason())
	}
}

// B1 regression: late append/snapshot calls after EndChannel must still accept periodicity checks.
// Real case: Anthropic may still send content_block_delta after content_block_stop,
// so scanning must not be skipped entirely just because the window was released.
func TestRequestGuardPostEndObservationsStillScanned(t *testing.T) {
	t.Run("AppendAfterEnd", func(t *testing.T) {
		rg := NewRequestGuard(defaultTestConfig())
		if err := rg.ObserveAppend("c_post", "", KindText, []byte("non-repeating prefix")); err != nil {
			t.Fatalf("prefix append error: %v", err)
		}
		if err := rg.EndChannel("c_post"); err != nil {
			t.Fatalf("EndChannel error: %v", err)
		}

		periodic := bytes.Repeat([]byte("0123456789"), 1024) // 10240 bytes, period 10
		var got error
		for off := 0; off < len(periodic); off += 1024 {
			end := off + 1024
			if end > len(periodic) {
				end = len(periodic)
			}
			got = rg.ObserveAppend("c_post", "", KindText, periodic[off:end])
			if got != nil {
				break
			}
		}
		if got == nil || got.Error() != "repetition" {
			t.Fatalf("post-end append must still trigger repetition detection, got %v", got)
		}
		if !rg.IsTripped() || rg.TrippedReason() != "repetition" {
			t.Fatalf("expected tripped with repetition, got %s", rg.TrippedReason())
		}
	})

	t.Run("SnapshotAfterEnd", func(t *testing.T) {
		rg := NewRequestGuard(defaultTestConfig())
		if err := rg.ObserveAppend("c_post_snap", "", KindText, []byte("x")); err != nil {
			t.Fatalf("append error: %v", err)
		}
		if err := rg.EndChannel("c_post_snap"); err != nil {
			t.Fatalf("EndChannel error: %v", err)
		}

		snapshot := bytes.Repeat([]byte("abcdef"), 1500) // 9000 bytes, period 6
		err := rg.ObserveSnapshot("c_post_snap", "", KindText, snapshot, "unknown_or_intermediate_type")
		if err == nil || err.Error() != "repetition" {
			t.Fatalf("post-end snapshot must still trigger repetition detection, got %v", err)
		}
		if !rg.IsTripped() || rg.TrippedReason() != "repetition" {
			t.Fatalf("expected tripped with repetition, got %s", rg.TrippedReason())
		}
	})
}

func TestRequestGuardRequestBytesPrecedenceOverScanBudget(t *testing.T) {
	// Spec §5.3 strict 8-step priority: step 4 semantic byte budget check (request_bytes) first, then step 6 scan budget
	rg := NewRequestGuard(Config{
		Enabled:               true,
		MaxToolArgumentBytes:  262144,
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       2097152,
		MaxRequestBytes:       1000, // only a 1000-byte request limit
	})

	// Pass a 1500-byte snapshot at once -> must report request_bytes, not channel_bytes or scan_budget
	err := rg.ObserveSnapshot("c1", "", KindText, make([]byte, 1500), "")
	if err == nil || err.Error() != "request_bytes" {
		t.Fatalf("expected request_bytes priority over scan budget, got %v", err)
	}
	// The first reason must be frozen
	if rg.TrippedReason() != "request_bytes" {
		t.Fatalf("expected frozen reason request_bytes, got %s", rg.TrippedReason())
	}

	// A repeated call still returns the first frozen reason
	err2 := rg.ObserveAppend("c1", "", KindText, []byte("x"))
	if err2 == nil || err2.Error() != "request_bytes" {
		t.Fatalf("expected idempotent frozen reason request_bytes, got %v", err2)
	}
}

func TestRequestGuardToolArgumentDeltasFlood(t *testing.T) {
	rg := NewRequestGuard(Config{
		Enabled:               true,
		MaxToolArgumentBytes:  262144,
		MaxToolArgumentDeltas: 10,
		MaxChannelBytes:       2097152,
		MaxRequestBytes:       4194304,
	})

	for i := 0; i < 10; i++ {
		if err := rg.ObserveAppend("call_1", "", KindToolArguments, []byte("a")); err != nil {
			t.Fatalf("delta %d error: %v", i, err)
		}
	}
	// The 11th non-empty delta must be blocked as tool_argument_deltas (Spec §5.3 step 3)
	err := rg.ObserveAppend("call_1", "", KindToolArguments, []byte("a"))
	if err == nil || err.Error() != "tool_argument_deltas" {
		t.Fatalf("expected tool_argument_deltas, got %v", err)
	}
}

func TestRequestGuardRecordEventAndNoEventBatching(t *testing.T) {
	rg := NewRequestGuard(defaultTestConfig())

	// 1. Test that a standalone RecordEvent counts exactly once per upstream business event
	for i := 0; i < MaxRequestEvents; i++ {
		if err := rg.RecordEvent(); err != nil {
			t.Fatalf("unexpected error at event %d: %v", i, err)
		}
	}
	// The 131073rd RecordEvent triggers the request_events decision and freezes it
	if err := rg.RecordEvent(); err == nil || err.Error() != "request_events" {
		t.Fatalf("expected request_events, got %v", err)
	}
	if !rg.IsTripped() || rg.TrippedReason() != "request_events" {
		t.Fatalf("expected tripped with request_events, got %s", rg.TrippedReason())
	}

	// 2. Test that several observations for the same event, using ObserveAppendNoEvent / ObserveSnapshotNoEvent, do not add to requestEvents again
	rgFresh := NewRequestGuard(defaultTestConfig())
	// Record 1 upstream business event
	if err := rgFresh.RecordEvent(); err != nil {
		t.Fatalf("RecordEvent failed: %v", err)
	}
	// One event derives 3 delta append observations; call ObserveAppendNoEvent
	for i := 0; i < 3; i++ {
		cid := fmt.Sprintf("chan_%d", i)
		if err := rgFresh.ObserveAppendNoEvent(cid, "", KindText, []byte("val")); err != nil {
			t.Fatalf("ObserveAppendNoEvent observation %d failed: %v", i, err)
		}
	}
	// Assert requestEvents is still only 1 (not added up to 4 by the 3 observations)
	rgFresh.mu.RLock()
	events := rgFresh.requestEvents
	rgFresh.mu.RUnlock()
	if events != 1 {
		t.Fatalf("expected requestEvents=1 for batch observations under single event, got %d", events)
	}
}

func TestRequestGuardLateArrivingAliasBidirectionalMerge(t *testing.T) {
	rg := NewRequestGuard(defaultTestConfig())
	// Simulate an early stream delta that contains only item_id = "item_123"
	if err := rg.ObserveAppend("item_123", "", KindToolArguments, []byte("part1")); err != nil {
		t.Fatalf("append with item_123 failed: %v", err)
	}
	// A later completion snapshot brings call_id = "call_abc"; the two are bound together
	if err := rg.ObserveSnapshot("call_abc", "item_123", KindToolArguments, []byte("part1part2"), "arguments.done"); err != nil {
		t.Fatalf("snapshot with call_abc and alias item_123 failed: %v", err)
	}
	// Verify ActiveChannelCount is still 1; it did not split into two channels
	if rg.ActiveChannelCount() != 1 {
		t.Fatalf("expected ActiveChannelCount=1 after alias merge, got %d", rg.ActiveChannelCount())
	}
	// Access again through item_123; it points to the same Channel
	if err := rg.ObserveSnapshot("item_123", "", KindToolArguments, []byte("part1part2"), "item.done"); err != nil {
		t.Fatalf("subsequent snapshot via item_123 failed: %v", err)
	}
	if rg.ActiveChannelCount() != 1 {
		t.Fatalf("expected ActiveChannelCount=1, got %d", rg.ActiveChannelCount())
	}
}

func TestRequestGuard_Concurrent_NoRace(t *testing.T) {
	rg := NewRequestGuard(defaultTestConfig())
	var wg sync.WaitGroup

	// Concurrent writes
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			cid := fmt.Sprintf("c_concurrent_%d", id%3)
			for j := 0; j < 50; j++ {
				_ = rg.ObserveAppend(cid, "", KindText, []byte("a"))
			}
		}(i)
	}

	// Concurrent state reads
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = rg.IsTripped()
				_ = rg.TrippedReason()
				_ = rg.ActiveChannelCount()
			}
		}()
	}

	wg.Wait()
}

func randBytes(n int, seed int64) []byte {
	b := make([]byte, n)
	rng := rand.New(rand.NewSource(seed))
	_, _ = rng.Read(b)
	return b
}

// TestInt64OverflowProtection:
// Verify that ChannelGuard.highwater and RequestGuard.requestBytes, when near math.MaxInt64,
// accept a positive delta without unprotected addition overflow flipping to a negative number and bypassing the budget check.
func TestInt64OverflowProtection(t *testing.T) {
	rg := NewRequestGuard(Config{
		Enabled:               true,
		MaxToolArgumentBytes:  262144,
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       2097152,
		MaxRequestBytes:       4194304,
	})

	_ = rg.ObserveAppend("c_overflow", "", KindText, []byte("init"))
	cg := rg.channels["c_overflow"]

	// Simulate a high-water mark near MaxInt64
	cg.highwater = math.MaxInt64 - 10
	rg.requestBytes = math.MaxInt64 - 10

	err := rg.ObserveAppend("c_overflow", "", KindText, make([]byte, 20))
	if err == nil {
		t.Fatalf("expected overflow rejection, but got nil (highwater/requestBytes wrapped around to negative!)")
	}
	if !rg.IsTripped() {
		t.Fatalf("expected guard to be tripped on overflow")
	}
}

// TestReviewProbeAppendScanBudgetVsRequestBytesPrecedence_RED:
// With a single channel configured, verify that on the append path step 4 (request_bytes) takes strict priority over step 6 (scan budget exhaustion).
func TestReviewProbeAppendScanBudgetVsRequestBytesPrecedence_RED(t *testing.T) {
	limit := int64(1 << 20)       // channel 1 MiB -> scan budget 2 MiB
	reqLimit := int64(600 * 1024) // request ≈ 0.586 MiB
	rg := NewRequestGuard(Config{
		Enabled:               true,
		MaxToolArgumentBytes:  262144,
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       limit,
		MaxRequestBytes:       reqLimit,
	})

	// 1) append 0.5MiB of non-repeated data -> requestBytes=0.5MiB, scan has 1.5MiB left, highwater=0.5MiB
	half := randBytes(512*1024, 9101)
	if err := rg.ObserveAppend("c1", "", KindText, half); err != nil {
		t.Fatalf("seed append failed: %v", err)
	}

	// 2) three 0.5MiB ineligible variant snapshots -> consume 1.5MiB of scan budget, leaving remaining scan budget = 0
	variant := make([]byte, 512*1024)
	copy(variant, half)
	for i := 0; i < 3; i++ {
		variant[i*100] ^= 0xAA
		if err := rg.ObserveSnapshot("c1", "", KindText, variant, ""); err != nil {
			t.Fatalf("variant snapshot %d failed: %v", i, err)
		}
	}
	if got := rg.TrippedReason(); got != "" {
		t.Fatalf("premature trip: %s", got)
	}

	// 3) append 0.2MiB: c1 highwater 0.7MiB <= 1MiB passes; scan budget 0 < 0.2MiB exceeds;
	//    at the same time requestBytes 0.5+0.2=0.7MiB > 0.586MiB also exceeds.
	//    Spec §5.3 strictly requires step 4 before step 6 -> the reason must be request_bytes!
	probe := randBytes(200*1024, 9102)
	err := rg.ObserveAppend("c1", "", KindText, probe)
	if err == nil {
		t.Fatalf("expected rejection, got nil")
	}
	if err.Error() != "request_bytes" {
		t.Fatalf("Spec §5.3 precedence violation: want 'request_bytes', got %q", err.Error())
	}
}

// TestAppendScanBudgetVsRequestBytesPrecedenceMultiChannelValidConfig:
// Legal configuration of the tool and channel bounds (tool <= channel <= request: tool=256KiB, channel=1MiB, request=1MiB).
// Occupy quota through another channel (c2) to construct precisely the priority race when several channels share the request budget.
func TestAppendScanBudgetVsRequestBytesPrecedenceMultiChannelValidConfig(t *testing.T) {
	channelLimit := int64(1 << 20) // 1 MiB
	reqLimit := int64(1 << 20)     // 1 MiB (legal: tool 256KiB <= channel 1MiB <= request 1MiB)
	rg := NewRequestGuard(Config{
		Enabled:               true,
		MaxToolArgumentBytes:  262144,
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       channelLimit,
		MaxRequestBytes:       reqLimit,
	})

	// 1) c1 append 0.5 MiB of non-repeated data -> c1 highwater=0.5MiB, c1 scan has 1.5MiB left, req=0.5MiB
	half := randBytes(512*1024, 9201)
	if err := rg.ObserveAppend("c1", "", KindText, half); err != nil {
		t.Fatalf("c1 seed append failed: %v", err)
	}

	// 2) c1 three variant snapshots consume 1.5 MiB of scan budget -> c1 remaining scan budget = 0, req unchanged (still 0.5MiB)
	variant := make([]byte, 512*1024)
	copy(variant, half)
	for i := 0; i < 3; i++ {
		variant[i*100] ^= 0x55
		if err := rg.ObserveSnapshot("c1", "", KindText, variant, ""); err != nil {
			t.Fatalf("variant snapshot %d failed: %v", i, err)
		}
	}

	// 3) c2 (another channel) append 0.4 MiB -> shared requestBytes accumulates to 0.5 + 0.4 = 0.9 MiB
	c2Data := randBytes(400*1024, 9202)
	if err := rg.ObserveAppend("c2", "", KindText, c2Data); err != nil {
		t.Fatalf("c2 append failed: %v", err)
	}

	// 4) c1 attempts to append 0.2 MiB:
	//    - c1 highwater: 0.5 + 0.2 = 0.7 MiB <= 1 MiB (channel_bytes passes)
	//    - requestBytes: 0.9 + 0.2 = 1.1 MiB > 1.0 MiB (request_bytes exceeds, step 4)
	//    - c1 scan budget: 0 < 0.2 MiB (scan budget insufficient, step 6)
	//    By Spec priority, it must report request_bytes, not channel_bytes!
	probe := randBytes(200*1024, 9203)
	err := rg.ObserveAppend("c1", "", KindText, probe)
	if err == nil {
		t.Fatalf("expected rejection, got nil")
	}
	if err.Error() != "request_bytes" {
		t.Fatalf("Multi-channel precedence violation: want 'request_bytes', got %q", err.Error())
	}
}

// TestBoundaryEndpointsAcceptance:
// Verify the R3-LOW-2 boundary-endpoint "accept direction" test:
// when each limit field is exactly equal to its legal maximum (pure > comparison):
// tool_argument_bytes == MaxToolArgumentBytes
// tool_argument_deltas == MaxToolArgumentDeltas
// channel_bytes == MaxChannelBytes
// request_bytes == MaxRequestBytes
// request_events == MaxRequestEvents
// max_request_channels == MaxRequestChannels
// all must be accepted and must not trigger any guard circuit breaker.
func TestBoundaryEndpointsAcceptance(t *testing.T) {
	t.Run("ToolArgumentBytesAndDeltasExactMaxAccepted", func(t *testing.T) {
		const maxDeltas = 10
		const maxBytes = 100
		rg := NewRequestGuard(Config{
			Enabled:               true,
			MaxToolArgumentBytes:  maxBytes,
			MaxToolArgumentDeltas: maxDeltas,
			MaxChannelBytes:       200,
			MaxRequestBytes:       1000,
		})

		// 10 deltas of 10 bytes each; cumulatively exactly 10 deltas and 100 bytes
		payload := randBytes(10, 1001)
		for i := 0; i < maxDeltas; i++ {
			if err := rg.ObserveAppend("call_boundary", "", KindToolArguments, payload); err != nil {
				t.Fatalf("delta %d of %d failed at exact boundary: %v", i+1, maxDeltas, err)
			}
		}

		if rg.IsTripped() {
			t.Fatalf("expected guard not tripped at exact tool bounds, but got: %s", rg.TrippedReason())
		}
	})

	t.Run("ChannelBytesAndRequestBytesExactMaxAccepted", func(t *testing.T) {
		const maxChannel = 500
		const maxRequest = 500
		rg := NewRequestGuard(Config{
			Enabled:               true,
			MaxToolArgumentBytes:  500,
			MaxToolArgumentDeltas: 100,
			MaxChannelBytes:       maxChannel,
			MaxRequestBytes:       maxRequest,
		})

		payload := randBytes(maxChannel, 1002)
		if err := rg.ObserveAppend("chan_boundary", "", KindText, payload); err != nil {
			t.Fatalf("expected exact channel/request max bytes to be accepted, got: %v", err)
		}

		if rg.IsTripped() {
			t.Fatalf("expected guard not tripped at exact channel/request bytes, but got: %s", rg.TrippedReason())
		}
	})

	t.Run("RequestEventsExactMaxAccepted", func(t *testing.T) {
		rg := NewRequestGuard(Config{
			Enabled:               true,
			MaxToolArgumentBytes:  1000000,
			MaxToolArgumentDeltas: 1000000,
			MaxChannelBytes:       1000000,
			MaxRequestBytes:       1000000,
		})

		for i := 0; i < MaxRequestEvents; i++ {
			if err := rg.RecordEvent(); err != nil {
				t.Fatalf("RecordEvent at exact boundary %d failed: %v", i+1, err)
			}
		}

		if rg.IsTripped() {
			t.Fatalf("expected guard not tripped at exact MaxRequestEvents, but got: %s", rg.TrippedReason())
		}
	})

	t.Run("MaxRequestChannelsExactMaxAccepted", func(t *testing.T) {
		rg := NewRequestGuard(Config{
			Enabled:               true,
			MaxToolArgumentBytes:  1000000,
			MaxToolArgumentDeltas: 1000000,
			MaxChannelBytes:       1000000,
			MaxRequestBytes:       1000000,
		})

		for i := 0; i < MaxRequestChannels; i++ {
			cid := fmt.Sprintf("chan_%d", i)
			if err := rg.ObserveAppend(cid, "", KindText, []byte("x")); err != nil {
				t.Fatalf("channel %d at exact boundary failed: %v", i+1, err)
			}
		}

		if rg.IsTripped() {
			t.Fatalf("expected guard not tripped at exact MaxRequestChannels, but got: %s", rg.TrippedReason())
		}
	})
}
