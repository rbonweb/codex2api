package outputguard

import (
	"crypto/sha256"
	"errors"
	"math"
)

var (
	ErrToolArgumentBytesExceeded = errors.New("tool_argument_bytes")
	ErrChannelBytesExceeded      = errors.New("channel_bytes")
	ErrScanBudgetExhausted       = errors.New("scan_budget_exhausted")
	ErrRepetitionDetected        = errors.New("repetition")
	ErrToolDeltasExceeded        = errors.New("tool_argument_deltas")
)

type ChannelKind string

const (
	KindText          ChannelKind = "text"
	KindThinking      ChannelKind = "thinking"
	KindToolArguments ChannelKind = "tool_arguments"
)

// allowedTerminalTypes is the allowlist of terminal types (Spec §5.3)
var allowedTerminalTypes = map[string]bool{
	"arguments.done":     true,
	"item.done":          true,
	"response.completed": true,
}

// postEndDetector keeps running bounded periodicity checks on later append/snapshot calls after the channel ends.
// It keeps only the 8 KiB ring window and the checkpoint cursor (it no longer keeps the 24 KiB linearized/prefix helper arrays),
// and borrows a temporary detector when it actually computes the period. This way late repeated data is still blocked after EndChannel,
// and avoids keeping a full RepetitionDetector in memory for every closed channel over the long term.
type postEndDetector struct {
	window                [WindowSizeBytes]byte
	count                 int64 // total bytes written (continuous with the detector before close)
	head                  int   // ring buffer write cursor (0..WindowSizeBytes-1)
	lastCheckedCheckpoint int64
}

// newPostEndDetector inherits the tail window and checkpoint state from the full detector before close,
// so window decisions after the end keep the same semantics as a window that was never released.
func newPostEndDetector(from *RepetitionDetector) *postEndDetector {
	d := &postEndDetector{}
	if from != nil {
		d.window = from.window
		d.count = from.count
		d.head = from.head
		d.lastCheckedCheckpoint = from.lastCheckedCheckpoint
	}
	return d
}

func (d *postEndDetector) Reset() {
	d.count = 0
	d.head = 0
	d.lastCheckedCheckpoint = 0
}

func (d *postEndDetector) writeChunk(chunk []byte) {
	n := len(chunk)
	if n == 0 {
		return
	}
	tailSpace := WindowSizeBytes - d.head
	if n <= tailSpace {
		copy(d.window[d.head:], chunk)
		d.head += n
		if d.head == WindowSizeBytes {
			d.head = 0
		}
	} else {
		copy(d.window[d.head:], chunk[:tailSpace])
		copy(d.window[:n-tailSpace], chunk[tailSpace:])
		d.head = n - tailSpace
	}
	d.count += int64(n)
}

func (d *postEndDetector) bytesNeededToNextCheckpoint() int {
	if d.count < WindowSizeBytes {
		return int(WindowSizeBytes - d.count)
	}
	nextCheckpoint := d.lastCheckedCheckpoint + CheckpointStepBytes
	if nextCheckpoint <= d.count {
		nextCheckpoint = d.count - (d.count % CheckpointStepBytes) + CheckpointStepBytes
	}
	return int(nextCheckpoint - d.count)
}

func (d *postEndDetector) advanceCheckpoint() {
	d.lastCheckedCheckpoint = d.count - (d.count % CheckpointStepBytes)
}

// Push keeps the same checkpoint semantics as RepetitionDetector.Push: it computes the period on the latest window only at checkpoints.
func (d *postEndDetector) Push(data []byte) (hit bool, period int) {
	for len(data) > 0 {
		needed := d.bytesNeededToNextCheckpoint()

		if len(data) < needed {
			d.writeChunk(data)
			return false, 0
		}

		d.writeChunk(data[:needed])
		data = data[needed:]
		d.advanceCheckpoint()
		if ok, p := d.checkCurrentWindow(); ok {
			return true, p
		}
	}
	return false, 0
}

// CheckTail has the same semantics as RepetitionDetector.CheckTail: it makes no decision when there is less than one full window.
func (d *postEndDetector) CheckTail() (hit bool, period int) {
	if d.count < WindowSizeBytes {
		return false, 0
	}
	return d.checkCurrentWindow()
}

// checkCurrentWindow linearizes the ring window and reuses the KMP period check of RepetitionDetector,
// to avoid duplicating the detection algorithm. The temporary detector is a fixed-size stack object and does not occupy the heap long term.
func (d *postEndDetector) checkCurrentWindow() (bool, int) {
	if d.count < WindowSizeBytes {
		return false, 0
	}
	var linear [WindowSizeBytes]byte
	tailLen := WindowSizeBytes - d.head
	copy(linear[:tailLen], d.window[d.head:])
	copy(linear[tailLen:], d.window[:d.head])

	var scratch RepetitionDetector
	return scratch.Push(linear[:])
}

type ChannelGuard struct {
	id                    string
	kind                  ChannelKind
	maxBytes              int64
	maxDeltas             int
	remainingScanBudget   int64
	highwater             int64
	deltaCount            int
	terminalBaselineHash  [sha256.Size]byte
	hasBaselineHash       bool
	terminalPermitsUsed   int
	consumedTerminalTypes map[string]bool
	detector              *RepetitionDetector
	postEnd               *postEndDetector
	isClosed              bool
}

func NewChannelGuard(id string, kind ChannelKind, maxBytes int64, maxDeltas int) *ChannelGuard {
	return &ChannelGuard{
		id:                    id,
		kind:                  kind,
		maxBytes:              maxBytes,
		maxDeltas:             maxDeltas,
		remainingScanBudget:   2 * maxBytes, // the cumulative scan budget is 2 * limit (Spec §5.3)
		consumedTerminalTypes: make(map[string]bool),
		detector:              NewRepetitionDetector(),
	}
}

func (c *ChannelGuard) Highwater() int64           { return c.highwater }
func (c *ChannelGuard) DeltaCount() int            { return c.deltaCount }
func (c *ChannelGuard) RemainingScanBudget() int64 { return c.remainingScanBudget }

// ReleaseWindow releases the full detection window but keeps the lightweight tail detector:
// late append/snapshot calls after the channel is closed must still accept bounded periodicity checks.
func (c *ChannelGuard) ReleaseWindow() {
	if c.isClosed {
		return
	}
	c.postEnd = newPostEndDetector(c.detector)
	c.detector = nil
	c.isClosed = true
}

// resetScanners resets the window state before a full-frame snapshot scan (the snapshot represents the complete state and is not concatenated with historical appends).
func (c *ChannelGuard) resetScanners() {
	if c.detector != nil {
		c.detector.Reset()
	}
	if c.postEnd != nil {
		c.postEnd.Reset()
	}
}

// scanPush feeds data to the currently available detector; after close it switches to the lightweight tail detector automatically.
func (c *ChannelGuard) scanPush(data []byte) (bool, int) {
	if c.detector != nil {
		return c.detector.Push(data)
	}
	if c.postEnd != nil {
		return c.postEnd.Push(data)
	}
	return false, 0
}

func (c *ChannelGuard) scanCheckTail() (bool, int) {
	if c.detector != nil {
		return c.detector.CheckTail()
	}
	if c.postEnd != nil {
		return c.postEnd.CheckTail()
	}
	return false, 0
}

func (c *ChannelGuard) CheckAppend(delta []byte) (growth int64, cost int64, err error) {
	deltaLen := int64(len(delta))
	if c.kind == KindToolArguments && deltaLen > 0 {
		if c.deltaCount+1 > c.maxDeltas {
			return 0, 0, ErrToolDeltasExceeded
		}
	}
	// int64 overflow protection: prevents addition overflow from flipping to a negative number and bypassing the budget check
	if deltaLen > 0 && c.highwater > math.MaxInt64-deltaLen {
		if c.kind == KindToolArguments {
			return 0, 0, ErrToolArgumentBytesExceeded
		}
		return 0, 0, ErrChannelBytesExceeded
	}
	newHighwater := c.highwater + deltaLen
	if newHighwater > c.maxBytes {
		if c.kind == KindToolArguments {
			return 0, 0, ErrToolArgumentBytesExceeded
		}
		return 0, 0, ErrChannelBytesExceeded
	}

	cost = deltaLen
	return deltaLen, cost, nil
}

func (c *ChannelGuard) CommitAppend(delta []byte, growth int64, cost int64) error {
	c.highwater += growth
	if c.kind == KindToolArguments && len(delta) > 0 {
		c.deltaCount++
	}
	if c.remainingScanBudget < cost {
		c.remainingScanBudget = 0
		return ErrScanBudgetExhausted
	}
	c.remainingScanBudget -= cost

	if len(delta) > 0 {
		// After close, the lightweight tail detector keeps scanning; EndChannel never skips periodicity checks.
		if hit, _ := c.scanPush(delta); hit {
			return ErrRepetitionDetected
		}
	}
	return nil
}

func (c *ChannelGuard) CheckSnapshot(snapshot []byte, terminalStage string) (growth int64, cost int64, skipRepetition bool, err error) {
	snapLen := int64(len(snapshot))
	if snapLen > c.highwater {
		growth = snapLen - c.highwater
	}
	if snapLen > c.maxBytes {
		if c.kind == KindToolArguments {
			return 0, 0, false, ErrToolArgumentBytesExceeded
		}
		return 0, 0, false, ErrChannelBytesExceeded
	}

	// Eligibility check: only an allowlisted terminal type that has a baseline hash and whose permit is not exhausted (Spec §5.3 step 5)
	// Legal and illegal terminal quota decisions happen strictly before hashing! Ineligible input never computes a hash early.
	isEligible := c.hasBaselineHash &&
		allowedTerminalTypes[terminalStage] &&
		!c.consumedTerminalTypes[terminalStage] &&
		c.terminalPermitsUsed < 3

	if isEligible {
		// Compute the collision-resistant digest of the snapshot and compare it with the baseline digest.
		// Use SHA-256 rather than 64-bit FNV: an attacker cannot build a snapshot with the same digest and different content at an acceptable cost.
		// Digest computation is still bound by the 2*limit scan budget and the 3 terminal permits (same as the old implementation; it does not add a full-text processing order of magnitude).
		currentDigest := sha256.Sum256(snapshot)
		if currentDigest == c.terminalBaselineHash {
			// The digest matches exactly; a pure terminal confirmation snapshot passes at zero cost and skips the step 7 repeat scan
			return growth, 0, true, nil
		}
		// The digest differs (variant tampering); revoke the permit for this type and charge the full cost
		cost = snapLen
	} else {
		// Not exempt (including an intermediate replacement, a first-frame terminal, a resend of the same type, or exceeding the 3-permit quota); never hash early, charge the full cost directly
		cost = snapLen
	}

	if c.remainingScanBudget < cost {
		return 0, 0, false, ErrScanBudgetExhausted
	}

	return growth, cost, false, nil
}

func (c *ChannelGuard) CommitSnapshot(snapshot []byte, growth int64, cost int64, terminalStage string) error {
	c.highwater += growth
	c.remainingScanBudget -= cost

	// An allowlisted terminal-type snapshot consumes the quota for its type
	if allowedTerminalTypes[terminalStage] {
		c.consumedTerminalTypes[terminalStage] = true
		c.terminalPermitsUsed++
	}

	// The first terminal frame establishes the baseline digest: only allowlisted terminal frame types may establish a baseline on the first frame (Spec §5.3).
	// Establishing the baseline does not depend on whether the detector is still alive: an allowlisted terminal snapshot after close also needs a baseline.
	shouldSetBaseline := !c.hasBaselineHash && allowedTerminalTypes[terminalStage]

	if cost > 0 {
		if shouldSetBaseline {
			c.terminalBaselineHash = sha256.Sum256(snapshot)
			c.hasBaselineHash = true
		}
		// The full-frame snapshot is scanned independently (after close the lightweight tail detector keeps running and no longer skips).
		c.resetScanners()
		if hit, _ := c.scanPush(snapshot); hit {
			return ErrRepetitionDetected
		}
		if hit, _ := c.scanCheckTail(); hit {
			return ErrRepetitionDetected
		}
	}

	return nil
}

func (c *ChannelGuard) CheckTail() error {
	if hit, _ := c.scanCheckTail(); hit {
		return ErrRepetitionDetected
	}
	return nil
}
