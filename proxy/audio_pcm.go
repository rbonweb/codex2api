package proxy

import (
	"encoding/binary"
	"errors"
)

// opusFrameSamples is one 20 ms Opus frame at 48 kHz mono.
const opusFrameSamples = 960

// decodePCM16LE converts little-endian 16-bit PCM bytes to samples. An odd length is an error.
func decodePCM16LE(b []byte) ([]int16, error) {
	if len(b)%2 != 0 {
		return nil, errors.New("pcm16 data has an odd number of bytes")
	}
	out := make([]int16, len(b)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(b[i*2:]))
	}
	return out, nil
}

// encodePCM16LE converts samples to little-endian 16-bit PCM bytes.
func encodePCM16LE(s []int16) []byte {
	out := make([]byte, len(s)*2)
	for i, v := range s {
		binary.LittleEndian.PutUint16(out[i*2:], uint16(v))
	}
	return out
}

// pcmUpsampler doubles a 24 kHz mono stream to 48 kHz by linear interpolation, continuous across calls.
// For each input sample it emits the midpoint of the previous and current sample, then the current sample; the very first sample pairs with itself.
type pcmUpsampler struct {
	prev    int16
	started bool
}

// Process upsamples one chunk. The output is exactly twice as long as the input.
func (u *pcmUpsampler) Process(in []int16) []int16 {
	out := make([]int16, 0, len(in)*2)
	for _, cur := range in {
		if !u.started {
			u.prev = cur
			u.started = true
		}
		out = append(out, int16((int32(u.prev)+int32(cur))>>1), cur)
		u.prev = cur
	}
	return out
}

// Reset forgets the previous sample, as at the start of a new stream.
func (u *pcmUpsampler) Reset() {
	u.prev = 0
	u.started = false
}

// resamplePCM16 resamples mono PCM once with linear interpolation. Equal rates return a copy.
func resamplePCM16(in []int16, fromRate, toRate int) []int16 {
	if len(in) == 0 || fromRate <= 0 || toRate <= 0 {
		return []int16{}
	}
	if fromRate == toRate {
		return append([]int16(nil), in...)
	}
	outLen := int(int64(len(in)) * int64(toRate) / int64(fromRate))
	if outLen == 0 {
		return []int16{}
	}
	out := make([]int16, outLen)
	last := len(in) - 1
	for i := range out {
		pos := int64(i) * int64(fromRate)
		idx := int(pos / int64(toRate))
		frac := float64(pos%int64(toRate)) / float64(toRate)
		if idx >= last {
			out[i] = in[last]
			continue
		}
		a, b := float64(in[idx]), float64(in[idx+1])
		out[i] = int16(a + (b-a)*frac)
	}
	return out
}

// opusFramer collects 48 kHz samples into complete 960-sample frames and keeps the remainder.
type opusFramer struct {
	buf []int16
}

// Push appends samples and returns every complete frame now available. Each returned frame is its own slice.
func (f *opusFramer) Push(samples []int16) [][]int16 {
	f.buf = append(f.buf, samples...)
	var frames [][]int16
	for len(f.buf) >= opusFrameSamples {
		frame := make([]int16, opusFrameSamples)
		copy(frame, f.buf[:opusFrameSamples])
		frames = append(frames, frame)
		f.buf = f.buf[opusFrameSamples:]
	}
	if len(f.buf) == 0 {
		f.buf = nil
	}
	return frames
}

// Flush pads the remainder with zeros to one frame. It returns nothing when no samples are pending.
func (f *opusFramer) Flush() [][]int16 {
	if len(f.buf) == 0 {
		return nil
	}
	frame := make([]int16, opusFrameSamples)
	copy(frame, f.buf)
	f.buf = nil
	return [][]int16{frame}
}

// Reset drops any pending samples.
func (f *opusFramer) Reset() {
	f.buf = nil
}

// Pending reports how many samples wait for a full frame.
func (f *opusFramer) Pending() int {
	return len(f.buf)
}
