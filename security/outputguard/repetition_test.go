package outputguard

import (
	"bytes"
	"math/rand"
	"testing"
)

func TestRepetitionDetectorNormalText(t *testing.T) {
	d := NewRepetitionDetector()
	// Pseudo-random non-periodic data
	r := rand.New(rand.NewSource(12345))
	nonRepeating := make([]byte, 10000)
	r.Read(nonRepeating)

	hit, _ := d.Push(nonRepeating)
	if hit {
		t.Fatalf("expected no repetition hit for pseudo-random data, got hit=true")
	}
}

func TestRepetitionDetectorShortPeriodHit(t *testing.T) {
	d := NewRepetitionDetector()
	// Create the real incident repetition pattern: period p = 30 ("_auditfree.md_pluginless.tmp.m")
	// Math check focus: 8192 % 30 = 22 != 0; verify that a finite string's period does not require L % p == 0!
	pattern := []byte("_auditfree.md_pluginless.tmp.m")
	if len(pattern) != 30 {
		t.Fatalf("pattern length = %d, want 30", len(pattern))
	}
	var stream []byte
	for len(stream) < 9500 {
		stream = append(stream, pattern...)
	}

	// Feed in 512-byte chunks
	hitFound := false
	var hitPeriod int
	for offset := 0; offset < len(stream); offset += 512 {
		end := offset + 512
		if end > len(stream) {
			end = len(stream)
		}
		hit, p := d.Push(stream[offset:end])
		if hit {
			hitFound = true
			hitPeriod = p
			break
		}
	}

	if !hitFound {
		t.Fatalf("expected repetition hit for 30-byte periodic pattern (non-multiple of 8192), got hit=false")
	}
	if hitPeriod != 30 {
		t.Fatalf("expected detected period 30, got %d", hitPeriod)
	}
}

func TestRepetitionDetectorTailCheckShortText(t *testing.T) {
	d := NewRepetitionDetector()
	// Short text < 8192 bytes: even if the content is highly periodic, the tail check must return hit=false (Spec §5.2 rule 6)
	pattern := bytes.Repeat([]byte("abc"), 500) // 1500 bytes
	hit, _ := d.Push(pattern)
	if hit {
		t.Fatalf("expected hit=false during push for <8192 bytes")
	}
	hit, _ = d.CheckTail()
	if hit {
		t.Fatalf("expected hit=false for tail check with <8192 bytes total")
	}
}

func TestRepetitionDetectorTailCheckUnalignedWindow(t *testing.T) {
	d := NewRepetitionDetector()
	// Build a real unaligned tail-check sample:
	// The first 108 bytes are a non-periodic stray prefix, followed by an 8192-byte repeated fragment with period 10; total length 8300 bytes.
	// During normal Push:
	// - When the total reaches 8192 bytes the checkpoint fires; the window then holds the 108-byte stray prefix plus 8084 bytes of periodic content, which is not a full period, so Push judges hit=false;
	// - The remaining 108 bytes are fed next (total 8300 bytes); the next 1024 checkpoint (9216 bytes) is not reached yet, so Push still does not hit;
	// - The stream ends and triggers CheckTail(): the latest 8192-byte window is made up exactly of period-10 data, so CheckTail must return hit=true, period=10 precisely.
	prefix := []byte("non-periodic-header-padding-1234567890-abcdefghijklmnopqrstuvwxyz!@#$%^&*()-random-preamble-bytes-to-offset-")
	if len(prefix) != 108 {
		t.Fatalf("prefix length = %d, want 108", len(prefix))
	}
	pattern := []byte("0123456789") // period 10
	periodicPart := bytes.Repeat(pattern, 8192/10)
	periodicPart = append(periodicPart, pattern[:8192%10]...) // exactly 8192 bytes of pure periodic data
	if len(periodicPart) != 8192 {
		t.Fatalf("periodicPart length = %d, want 8192", len(periodicPart))
	}

	fullStream := append(prefix, periodicPart...) // total 108 + 8192 = 8300 bytes

	// Push in chunks and assert that Push never hits (the 8192 checkpoint includes the prefix, and the next 9216 checkpoint is not reached)
	for offset := 0; offset < len(fullStream); offset += 500 {
		end := offset + 500
		if end > len(fullStream) {
			end = len(fullStream)
		}
		hit, _ := d.Push(fullStream[offset:end])
		if hit {
			t.Fatalf("Push prematurely hit at offset %d, expected tail check to detect", offset)
		}
	}

	// Trigger the tail check
	hit, p := d.CheckTail()
	if !hit {
		t.Fatalf("expected CheckTail to detect pure periodic window of latest 8192 bytes, got hit=false")
	}
	if p != 10 {
		t.Fatalf("expected detected period 10, got %d", p)
	}
}

func TestRepetitionDetectorConsecutive9215BytesGuaranteedHit(t *testing.T) {
	d := NewRepetitionDetector()
	// Spec §5.2 rule 3: step 1024B; a contiguous periodic fragment of any starting point that reaches 9215B always has a checkpoint falling entirely inside it
	prefix := make([]byte, 512) // arbitrary offset prefix of 512B
	for i := range prefix {
		prefix[i] = byte(i % 127)
	}
	pattern := []byte("repeated_segment_32_bytes_test_")
	periodicBody := bytes.Repeat(pattern, 300) // > 9215B
	stream := append(prefix, periodicBody...)

	hit := false
	for offset := 0; offset < len(stream); offset += 256 {
		end := offset + 256
		if end > len(stream) {
			end = len(stream)
		}
		if ok, _ := d.Push(stream[offset:end]); ok {
			hit = true
			break
		}
	}
	if !hit {
		t.Fatalf("expected 9215-byte periodic segment to trigger repetition hit within checkpoints")
	}
}

func TestRepetitionDetectorPushWithHashPipelined(t *testing.T) {
	d := NewRepetitionDetector()
	data := []byte("abcdefghijklmnopqrstuvwxyz0123456789")
	const fnvOffset uint64 = 14695981039346656037
	hit, p, finalHash := d.PushWithHash(data, fnvOffset)
	if hit || p != 0 {
		t.Fatalf("unexpected hit on short non-repeating data: hit=%v, p=%d", hit, p)
	}
	// Compute the expected FNV-1a hash
	var expectedHash uint64 = fnvOffset
	for _, b := range data {
		expectedHash = (expectedHash ^ uint64(b)) * 1099511628211
	}
	if finalHash != expectedHash {
		t.Fatalf("finalHash want %d, got %d", expectedHash, finalHash)
	}
}
