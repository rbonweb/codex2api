package outputguard

const (
	WindowSizeBytes     = 8192
	CheckpointStepBytes = 1024
	MaxRepetitionPeriod = 256
	MinRepetitionPeriod = 1
)

type RepetitionDetector struct {
	window                [WindowSizeBytes]byte
	linear                [WindowSizeBytes]byte
	pi                    [WindowSizeBytes]uint16
	count                 int64 // total bytes ever written to the channel
	head                  int   // ring buffer write cursor (0..8191)
	lastCheckedCheckpoint int64
}

func NewRepetitionDetector() *RepetitionDetector {
	return &RepetitionDetector{}
}

func (d *RepetitionDetector) Reset() {
	d.count = 0
	d.head = 0
	d.lastCheckedCheckpoint = 0
}

func (d *RepetitionDetector) writeChunk(chunk []byte) {
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

func (d *RepetitionDetector) bytesNeededToNextCheckpoint() int {
	if d.count < WindowSizeBytes {
		return int(WindowSizeBytes - d.count)
	}
	nextCheckpoint := d.lastCheckedCheckpoint + CheckpointStepBytes
	if nextCheckpoint <= d.count {
		nextCheckpoint = d.count - (d.count % CheckpointStepBytes) + CheckpointStepBytes
	}
	return int(nextCheckpoint - d.count)
}

func (d *RepetitionDetector) advanceCheckpoint() {
	d.lastCheckedCheckpoint = d.count - (d.count % CheckpointStepBytes)
}

func (d *RepetitionDetector) Push(data []byte) (hit bool, period int) {
	for len(data) > 0 {
		needed := d.bytesNeededToNextCheckpoint()

		if len(data) < needed {
			d.writeChunk(data)
			return false, 0
		}

		// Checkpoint reached
		d.writeChunk(data[:needed])
		data = data[needed:]
		d.advanceCheckpoint()
		if ok, p := d.checkCurrentWindow(); ok {
			return true, p
		}
	}
	return false, 0
}

// PushWithHash computes the FNV-1a hash and runs the sliding-window repetition check in one pipelined pass (Spec §5.3)
func (d *RepetitionDetector) PushWithHash(data []byte, initialHash uint64) (hit bool, period int, finalHash uint64) {
	h := initialHash
	for len(data) > 0 {
		needed := d.bytesNeededToNextCheckpoint()

		if len(data) < needed {
			for _, b := range data {
				h = (h ^ uint64(b)) * 1099511628211
			}
			d.writeChunk(data)
			return false, 0, h
		}

		chunk := data[:needed]
		for _, b := range chunk {
			h = (h ^ uint64(b)) * 1099511628211
		}
		d.writeChunk(chunk)
		data = data[needed:]
		d.advanceCheckpoint()
		if ok, p := d.checkCurrentWindow(); ok {
			return true, p, h
		}
	}
	return false, 0, h
}

func (d *RepetitionDetector) CheckTail() (hit bool, period int) {
	// Spec §5.2 rule 6: if the channel's total bytes written are < 8192, do not judge repetition; pass directly
	if d.count < WindowSizeBytes {
		return false, 0
	}
	// The window is full at 8192 but the tail is not aligned to 1024; extract the latest complete 8192 window and run one period check
	return d.checkCurrentWindow()
}

// checkCurrentWindow extracts the linear 8192 bytes from the current ring buffer and uses the prefix function to find the shortest period 1 <= p <= 256
// Math check: a finite string's period does not require L % p == 0 (for example, incident period p=30, 8192 % 30 = 22 != 0).
// The maximum border k = pi[L-1] of the prefix function corresponds to the minimum period p = L - k.
// A hit is declared if and only if 1 <= p <= 256 and window[i] == window[i-p] holds for all p <= i < 8192.
// Periods are inherited by prefixes: if the minimum period (i+1)-pi[i] of the prefix [0..i] is > 256, the whole window cannot have p <= 256, so it can exit early.
func (d *RepetitionDetector) checkCurrentWindow() (bool, int) {
	if d.count < WindowSizeBytes {
		return false, 0
	}
	// Unfold the current latest 8192-byte contiguous view: use copy instead of byte-by-byte modular unfolding
	tailLen := WindowSizeBytes - d.head
	copy(d.linear[:tailLen], d.window[d.head:])
	copy(d.linear[tailLen:], d.window[:d.head])

	// Use a streaming KMP prefix function and exit early when the minimum period exceeds MaxRepetitionPeriod
	d.pi[0] = 0
	for i := 1; i < WindowSizeBytes; i++ {
		j := int(d.pi[i-1])
		for j > 0 && d.linear[i] != d.linear[j] {
			j = int(d.pi[j-1])
		}
		if d.linear[i] == d.linear[j] {
			j++
		}
		d.pi[i] = uint16(j)

		// Early exit by prefix inheritance of periods: the shortest period of the prefix [0..i] is (i+1) - pi[i]
		// If the prefix's shortest period already exceeds MaxRepetitionPeriod (256), the shortest period of the whole 8192 window containing this prefix must be > 256
		if (i+1)-j > MaxRepetitionPeriod {
			return false, 0
		}
	}

	k := int(d.pi[WindowSizeBytes-1])
	p := WindowSizeBytes - k
	if p >= MinRepetitionPeriod && p <= MaxRepetitionPeriod {
		return true, p
	}

	return false, 0
}
