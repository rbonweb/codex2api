package outputguard

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"math/rand"
	"testing"
)

func TestChannelGuardHighwaterGrowthAndScanBudget(t *testing.T) {
	// tool channel: maxBytes = 256 KiB (262144), scanBudget = 2 * 256 KiB = 512 KiB
	cg := NewChannelGuard("call_1", KindToolArguments, 262144, 4096)

	// Append 64 KiB of non-repeating data
	delta1 := make([]byte, 65536)
	r1 := rand.New(rand.NewSource(10001))
	r1.Read(delta1)

	growth, cost, err := cg.CheckAppend(delta1)
	if err != nil {
		t.Fatalf("CheckAppend delta1 error: %v", err)
	}
	if growth != 65536 || cost != 65536 {
		t.Fatalf("delta1: want growth=65536, cost=65536, got growth=%d, cost=%d", growth, cost)
	}
	if err := cg.CommitAppend(delta1, growth, cost); err != nil {
		t.Fatalf("CommitAppend delta1 error: %v", err)
	}

	if cg.Highwater() != 65536 {
		t.Fatalf("want highwater=65536, got %d", cg.Highwater())
	}
	if cg.RemainingScanBudget() != 524288-65536 {
		t.Fatalf("want remainingScanBudget=%d, got %d", 524288-65536, cg.RemainingScanBudget())
	}
}

// Spec §9.1 Case 8a: a variant snapshot that exceeds the budget is blocked after the scan budget is exhausted
func TestChannelGuardCase8a_ScanBudgetExhaustionAndVariantRejection(t *testing.T) {
	cg := NewChannelGuard("call_1", KindToolArguments, 262144, 4096)
	// 256 deltas of 1 KiB exhaust the 256 KiB scan budget (256 KiB remaining)
	chunk := make([]byte, 1024)
	r2 := rand.New(rand.NewSource(20002))
	r2.Read(chunk)
	var fullParam []byte
	for i := 0; i < 256; i++ {
		fullParam = append(fullParam, chunk...)
		growth, cost, err := cg.CheckAppend(chunk)
		if err != nil {
			t.Fatalf("step %d append error: %v", i, err)
		}
		if err := cg.CommitAppend(chunk, growth, cost); err != nil {
			t.Fatalf("step %d commit append error: %v", i, err)
		}
	}
	if cg.RemainingScanBudget() != 262144 {
		t.Fatalf("remaining scan budget want 262144, got %d", cg.RemainingScanBudget())
	}

	// The first 256 KiB allowlisted terminal snapshot arrives (arguments.done); there is no baseline hash yet, cost = 256 KiB
	growth, cost, skipRepetition, err := cg.CheckSnapshot(fullParam, "arguments.done")
	if err != nil {
		t.Fatalf("arguments.done snapshot error: %v", err)
	}
	if cost != 262144 || growth != 0 || skipRepetition {
		t.Fatalf("arguments.done: want cost=262144, growth=0, skipRepetition=false; got cost=%d, growth=%d, skip=%v", cost, growth, skipRepetition)
	}
	if err := cg.CommitSnapshot(fullParam, growth, cost, "arguments.done"); err != nil {
		t.Fatalf("arguments.done commit snapshot error: %v", err)
	}

	if cg.RemainingScanBudget() != 0 {
		t.Fatalf("remaining scan budget want 0, got %d", cg.RemainingScanBudget())
	}

	// Immediately after, a 256 KiB variant snapshot with 1 byte changed arrives (item.done)
	variantParam := bytes.Clone(fullParam)
	variantParam[65536] ^= 0xFF

	_, _, _, err = cg.CheckSnapshot(variantParam, "item.done")
	if err == nil {
		t.Fatalf("expected variant snapshot to be rejected with scan budget exhaustion, got nil")
	}
	if err != ErrScanBudgetExhausted {
		t.Fatalf("expected ErrScanBudgetExhausted, got %v", err)
	}
}

// Spec §9.1 Case 8b: a legal terminal chain passes at zero cost, and ineligible calls are rejected
func TestChannelGuardCase8b_TerminalChainZeroCostAndExhaustion(t *testing.T) {
	cg := NewChannelGuard("call_1", KindToolArguments, 262144, 4096)
	fullParam := make([]byte, 262144)
	r3 := rand.New(rand.NewSource(30003))
	r3.Read(fullParam)
	// append 256 KiB
	growth, cost, _ := cg.CheckAppend(fullParam)
	_ = cg.CommitAppend(fullParam, growth, cost)

	// arguments.done consumes the last 256 KiB of the scan budget and establishes the baseline hash
	growth, cost, _, _ = cg.CheckSnapshot(fullParam, "arguments.done")
	_ = cg.CommitSnapshot(fullParam, growth, cost, "arguments.done")

	// item.done is eligible and the hash matches exactly -> cost = 0, skipRepetition = true
	growth, cost, skipRepetition, err := cg.CheckSnapshot(fullParam, "item.done")
	if err != nil {
		t.Fatalf("item.done snapshot error: %v", err)
	}
	if cost != 0 || !skipRepetition {
		t.Fatalf("item.done: want cost=0, skipRepetition=true; got cost=%d, skip=%v", cost, skipRepetition)
	}
	_ = cg.CommitSnapshot(fullParam, growth, cost, "item.done")

	// response.completed is likewise eligible and the hash matches -> cost = 0
	growth, cost, skipRepetition, err = cg.CheckSnapshot(fullParam, "response.completed")
	if err != nil {
		t.Fatalf("response.completed snapshot error: %v", err)
	}
	if cost != 0 || !skipRepetition {
		t.Fatalf("response.completed: want cost=0, skipRepetition=true")
	}
	_ = cg.CommitSnapshot(fullParam, growth, cost, "response.completed")

	// Resending the same type response.completed -> loses eligibility and takes cost = 256 KiB; it is blocked because the remaining scan budget is 0
	_, _, _, err = cg.CheckSnapshot(fullParam, "response.completed")
	if err != ErrScanBudgetExhausted {
		t.Fatalf("duplicate response.completed: want ErrScanBudgetExhausted, got %v", err)
	}
}

// Spec §9.1 Case 8c: an ordinary non-allowlisted snapshot never establishes a baseline
func TestChannelGuardCase8c_NonTerminalSnapshotCannotEstablishBaseline(t *testing.T) {
	cg := NewChannelGuard("c_text", KindText, 2097152, 4096)
	snap := []byte("intermediate full snapshot text")
	growth, cost, skip, err := cg.CheckSnapshot(snap, "unknown_or_intermediate_type")
	if err != nil {
		t.Fatalf("CheckSnapshot error: %v", err)
	}
	if skip || cost != int64(len(snap)) {
		t.Fatalf("want skip=false, cost=%d, got skip=%v, cost=%d", len(snap), skip, cost)
	}
	_ = cg.CommitSnapshot(snap, growth, cost, "unknown_or_intermediate_type")

	// Confirm hasBaselineHash is still false; no baseline was established
	if cg.hasBaselineHash {
		t.Fatalf("expected hasBaselineHash=false for non-terminal snapshot, got true")
	}
}

func TestChannelGuardHashMismatchDiscardsQuotaAndChargesFullCost(t *testing.T) {
	cg := NewChannelGuard("c_tool", KindToolArguments, 262144, 4096)
	data1 := bytes.Repeat([]byte("hello world "), 100) // 1200 bytes
	growth, cost, _, _ := cg.CheckSnapshot(data1, "arguments.done")
	_ = cg.CommitSnapshot(data1, growth, cost, "arguments.done")

	if !cg.hasBaselineHash {
		t.Fatalf("expected baseline hash created")
	}

	// Immediately send a tampered variant snapshot
	data2 := bytes.Repeat([]byte("corrupted data!"), 80) // 1200 bytes
	growth, cost, skip, err := cg.CheckSnapshot(data2, "item.done")
	if err != nil {
		t.Fatalf("unexpected CheckSnapshot error: %v", err)
	}
	if skip {
		t.Fatalf("expected skip=false for hash mismatch")
	}
	if cost != int64(len(data2)) {
		t.Fatalf("expected full cost %d, got %d", len(data2), cost)
	}

	// After CommitSnapshot tampers with the frame, the permit for this type must be marked revoked
	_ = cg.CommitSnapshot(data2, growth, cost, "item.done")
	if !cg.consumedTerminalTypes["item.done"] {
		t.Fatalf("expected item.done marked as consumed/discarded")
	}
	// The baseline digest must never be overwritten by a tampered snapshot, and it must be the collision-resistant SHA-256 (32B) rather than 64-bit FNV
	wantDigest := sha256.Sum256(data1)
	if cg.terminalBaselineHash != wantDigest {
		t.Fatalf("expected baseline sha256 digest to stay unchanged")
	}
	if cg.terminalBaselineHash == sha256.Sum256(data2) {
		t.Fatalf("expected tampered snapshot to have a different sha256 digest")
	}
}

// B2 regression: terminal equivalence comparison uses the collision-resistant SHA-256 digest.
// The old implementation used 64-bit FNV-1a, which let an attacker use collisions to pass a modified snapshot at zero cost;
// here the selectivity of the SHA-256 path is verified directly: a single-byte tamper of the same length must produce a different digest and be charged in full.
func TestChannelGuardTerminalBaselineDigestIsCollisionResistant(t *testing.T) {
	cg := NewChannelGuard("c_digest", KindToolArguments, 262144, 4096)
	data1 := bytes.Repeat([]byte("terminal-snapshot-"), 64)
	growth, cost, _, err := cg.CheckSnapshot(data1, "arguments.done")
	if err != nil {
		t.Fatalf("first snapshot CheckSnapshot error: %v", err)
	}
	if err := cg.CommitSnapshot(data1, growth, cost, "arguments.done"); err != nil {
		t.Fatalf("first snapshot CommitSnapshot error: %v", err)
	}

	if !cg.hasBaselineHash {
		t.Fatalf("expected baseline digest created")
	}
	if len(cg.terminalBaselineHash) != sha256.Size {
		t.Fatalf("baseline digest must be %d bytes (sha256), got %d", sha256.Size, len(cg.terminalBaselineHash))
	}
	if cg.terminalBaselineHash != sha256.Sum256(data1) {
		t.Fatalf("baseline digest must equal sha256(snapshot)")
	}

	variant := bytes.Clone(data1)
	variant[len(variant)-1] ^= 0x01
	if sha256.Sum256(variant) == cg.terminalBaselineHash {
		t.Fatalf("sha256 digest collision on single-byte mutation is unexpected")
	}

	growth, cost, skip, err := cg.CheckSnapshot(variant, "item.done")
	if err != nil {
		t.Fatalf("variant CheckSnapshot error: %v", err)
	}
	if skip {
		t.Fatalf("mutated variant must not be treated as equivalent terminal snapshot")
	}
	if cost != int64(len(variant)) {
		t.Fatalf("mutated variant must be fully charged, want %d got %d", len(variant), cost)
	}
}

// B1 regression: a late append after the channel ends (ReleaseWindow) must still accept periodicity checks.
// This matches the case where Anthropic still sends content_block_delta after content_block_stop.
func TestChannelGuardPostEndAppendStillScans(t *testing.T) {
	cg := NewChannelGuard("c_post_append", KindText, 1<<20, 4096)
	prefix := []byte("non-repeating-prefix-")
	growth, cost, err := cg.CheckAppend(prefix)
	if err != nil {
		t.Fatalf("prefix CheckAppend error: %v", err)
	}
	if err := cg.CommitAppend(prefix, growth, cost); err != nil {
		t.Fatalf("prefix CommitAppend error: %v", err)
	}

	cg.ReleaseWindow()
	if cg.isClosed != true || cg.detector != nil || cg.postEnd == nil {
		t.Fatalf("ReleaseWindow must close channel and switch to bounded post-end detector")
	}

	periodic := bytes.Repeat([]byte("0123456789"), 1024) // 10240 bytes, period 10
	var scanErr error
	for off := 0; off < len(periodic); off += 1024 {
		end := off + 1024
		if end > len(periodic) {
			end = len(periodic)
		}
		chunk := periodic[off:end]
		growth, cost, err := cg.CheckAppend(chunk)
		if err != nil {
			t.Fatalf("post-end CheckAppend error: %v", err)
		}
		if err := cg.CommitAppend(chunk, growth, cost); err != nil {
			scanErr = err
			break
		}
	}
	if !errors.Is(scanErr, ErrRepetitionDetected) {
		t.Fatalf("post-end append must still trigger repetition detection, got %v", scanErr)
	}
}

// B1 regression: a snapshot after the channel ends must still accept periodicity checks.
func TestChannelGuardPostEndSnapshotStillScans(t *testing.T) {
	cg := NewChannelGuard("c_post_snapshot", KindText, 1<<20, 4096)
	cg.ReleaseWindow()

	snapshot := bytes.Repeat([]byte("abcdef"), 1500) // 9000 bytes, period 6
	growth, cost, skip, err := cg.CheckSnapshot(snapshot, "unknown_or_intermediate_type")
	if err != nil {
		t.Fatalf("post-end CheckSnapshot error: %v", err)
	}
	if skip {
		t.Fatalf("non-terminal snapshot must not be treated as equivalent")
	}
	if err := cg.CommitSnapshot(snapshot, growth, cost, "unknown_or_intermediate_type"); !errors.Is(err, ErrRepetitionDetected) {
		t.Fatalf("post-end snapshot must still trigger repetition detection, got %v", err)
	}
}

// B1 regression: scanning after the end is still bound by the 2*limit scan budget; closing the channel must not be used to bypass the budget.
func TestChannelGuardPostEndScanBudgetStillEnforced(t *testing.T) {
	cg := NewChannelGuard("c_post_budget", KindText, 4096, 4096)
	if cg.RemainingScanBudget() != 8192 {
		t.Fatalf("want initial scan budget 8192, got %d", cg.RemainingScanBudget())
	}

	r := rand.New(rand.NewSource(424242))
	full := make([]byte, 4096)
	r.Read(full)
	growth, cost, err := cg.CheckAppend(full)
	if err != nil {
		t.Fatalf("pre-end CheckAppend error: %v", err)
	}
	if err := cg.CommitAppend(full, growth, cost); err != nil {
		t.Fatalf("pre-end CommitAppend error: %v", err)
	}
	if cg.RemainingScanBudget() != 4096 {
		t.Fatalf("pre-end append must consume 4096 budget, remaining=%d", cg.RemainingScanBudget())
	}

	cg.ReleaseWindow()

	// Snapshot scans after the end also deduct from the budget (non-repeated data does not trigger detection, but it must be charged).
	growth, cost, _, err = cg.CheckSnapshot(full, "unknown_or_intermediate_type")
	if err != nil {
		t.Fatalf("post-end CheckSnapshot error: %v", err)
	}
	if err := cg.CommitSnapshot(full, growth, cost, "unknown_or_intermediate_type"); err != nil {
		t.Fatalf("post-end CommitSnapshot error: %v", err)
	}
	if cg.RemainingScanBudget() != 0 {
		t.Fatalf("post-end snapshot must consume remaining budget, remaining=%d", cg.RemainingScanBudget())
	}

	// After the budget is exhausted, snapshots after the end must not get free scans.
	if _, _, _, err := cg.CheckSnapshot(full, "unknown_or_intermediate_type"); !errors.Is(err, ErrScanBudgetExhausted) {
		t.Fatalf("post-end scan must fail once budget exhausted, got %v", err)
	}
}

func TestChannelGuardTailCheck(t *testing.T) {
	cg := NewChannelGuard("c_text", KindText, 2097152, 4096)
	// Short text passes
	_ = cg.CommitAppend([]byte("short text"), 10, 10)
	if err := cg.CheckTail(); err != nil {
		t.Fatalf("expected nil error for short text tail check, got %v", err)
	}

	// Long-period text is detected
	cg2 := NewChannelGuard("c_periodic", KindText, 2097152, 4096)
	pattern := []byte("0123456789")
	data := bytes.Repeat(pattern, 850) // 8500 bytes
	// Avoid triggering a checkpoint in append; write directly into the window
	cg2.detector.window = [8192]byte{}
	for i := 0; i < len(data); i++ {
		cg2.detector.window[i%8192] = data[i]
	}
	cg2.detector.count = int64(len(data))
	cg2.detector.head = len(data) % 8192

	if err := cg2.CheckTail(); err != ErrRepetitionDetected {
		t.Fatalf("expected ErrRepetitionDetected for periodic tail check, got %v", err)
	}
}
