package proxy

import (
	"fmt"
)

// clientAudioFormat is one client audio format; one format applies to input and output.
type clientAudioFormat struct {
	Type string // "audio/pcm", "audio/pcmu" or "audio/pcma"
	Rate int    // 24000 or 16000 for audio/pcm; 8000 for G.711
}

// clientAudioFormatDefault is the internal stream format: 24 kHz mono PCM16 little-endian.
var clientAudioFormatDefault = clientAudioFormat{Type: "audio/pcm", Rate: 24000}

// parseClientAudioFormat validates a client audio format. A rate of 0 means the type's default (24000 for audio/pcm, 8000 for G.711). The second return value names the bad field ("type" or "rate") and is empty on success.
func parseClientAudioFormat(typ string, rate int) (clientAudioFormat, string, error) {
	switch typ {
	case "audio/pcm":
		if rate == 0 {
			rate = 24000
		}
		if rate != 24000 && rate != 16000 {
			return clientAudioFormat{}, "rate", fmt.Errorf("rate %d is not supported for audio/pcm; use 24000 or 16000", rate)
		}
	case "audio/pcmu", "audio/pcma":
		if rate == 0 {
			rate = 8000
		}
		if rate != 8000 {
			return clientAudioFormat{}, "rate", fmt.Errorf("rate %d is not supported for %s; use 8000", rate, typ)
		}
	default:
		return clientAudioFormat{}, "type", fmt.Errorf("audio type %q is not supported; use audio/pcm, audio/pcmu or audio/pcma", typ)
	}
	return clientAudioFormat{Type: typ, Rate: rate}, "", nil
}

// pcmByteCarry holds an odd trailing byte of PCM16 data until the next call.
type pcmByteCarry struct {
	pending []byte
}

// take returns the whole-sample prefix of the pending byte plus in and keeps an odd trailing byte for the next call. The result never aliases in.
func (c *pcmByteCarry) take(in []byte) []byte {
	buf := make([]byte, 0, len(c.pending)+len(in))
	buf = append(buf, c.pending...)
	buf = append(buf, in...)
	c.pending = nil
	if len(buf)%2 == 1 {
		c.pending = []byte{buf[len(buf)-1]}
		buf = buf[:len(buf)-1]
	}
	return buf
}

// clientAudioDecoder turns client audio bytes into 24 kHz PCM16LE bytes. It is stateful: feeding a stream in any chunking gives the same output as one call, except for a trailing sample held for interpolation or an odd byte held for the next call.
type clientAudioDecoder struct {
	format clientAudioFormat
	carry  pcmByteCarry
	rs     *pcmStreamResampler
}

// newClientAudioDecoder builds a decoder for one validated client format.
func newClientAudioDecoder(f clientAudioFormat) *clientAudioDecoder {
	d := &clientAudioDecoder{format: f}
	if f.Rate != 24000 {
		d.rs = newPCMStreamResampler(f.Rate, 24000)
	}
	return d
}

// Decode converts client audio bytes to 24 kHz PCM16LE bytes.
func (d *clientAudioDecoder) Decode(in []byte) []byte {
	switch d.format.Type {
	case "audio/pcmu", "audio/pcma":
		samples := make([]int16, len(in))
		for i, b := range in {
			if d.format.Type == "audio/pcmu" {
				samples[i] = muLawDecode(b)
			} else {
				samples[i] = aLawDecode(b)
			}
		}
		return encodePCM16LE(d.rs.Process(samples))
	}
	pcm := d.carry.take(in)
	if d.rs == nil {
		return pcm
	}
	samples, _ := decodePCM16LE(pcm) // take keeps the length even, so this cannot fail
	return encodePCM16LE(d.rs.Process(samples))
}

// clientAudioEncoder turns 24 kHz PCM16LE bytes into client audio bytes. It is stateful like the decoder.
type clientAudioEncoder struct {
	format clientAudioFormat
	carry  pcmByteCarry
	rs     *pcmStreamResampler
}

// newClientAudioEncoder builds an encoder for one validated client format.
func newClientAudioEncoder(f clientAudioFormat) *clientAudioEncoder {
	e := &clientAudioEncoder{format: f}
	if f.Rate != 24000 {
		e.rs = newPCMStreamResampler(24000, f.Rate)
	}
	return e
}

// Encode converts 24 kHz PCM16LE bytes to client audio bytes.
func (e *clientAudioEncoder) Encode(pcm24 []byte) []byte {
	pcm := e.carry.take(pcm24)
	samples, _ := decodePCM16LE(pcm) // take keeps the length even, so this cannot fail
	if e.rs != nil {
		samples = e.rs.Process(samples)
	}
	switch e.format.Type {
	case "audio/pcmu", "audio/pcma":
		out := make([]byte, len(samples))
		for i, s := range samples {
			if e.format.Type == "audio/pcmu" {
				out[i] = muLawEncode(s)
			} else {
				out[i] = aLawEncode(s)
			}
		}
		return out
	}
	return encodePCM16LE(samples)
}

// muLawEncode converts one 16-bit sample to a G.711 mu-law byte (ITU-T G.711, bias 0x84, clip 32635).
func muLawEncode(s int16) byte {
	const bias = 0x84
	const clip = 32635
	v := int32(s)
	mask := byte(0xFF)
	if v < 0 {
		v = -v
		mask = 0x7F
	}
	if v > clip {
		v = clip
	}
	v += bias
	seg := 0
	for seg < 7 && v >= 0x100<<seg {
		seg++
	}
	q := byte((v >> (seg + 3)) & 0x0F)
	return (byte(seg)<<4 | q) ^ mask
}

// muLawDecode converts one G.711 mu-law byte to a 16-bit sample.
func muLawDecode(b byte) int16 {
	const bias = 0x84
	u := ^b
	t := (int32(u&0x0F) << 3) + bias
	t <<= (u & 0x70) >> 4
	if u&0x80 != 0 {
		return int16(bias - t)
	}
	return int16(t - bias)
}

// aLawEncode converts one 16-bit sample to a G.711 A-law byte (ITU-T G.711).
func aLawEncode(s int16) byte {
	aend := [8]int32{0x1F, 0x3F, 0x7F, 0xFF, 0x1FF, 0x3FF, 0x7FF, 0xFFF}
	v := int32(s) >> 3
	mask := byte(0xD5)
	if v < 0 {
		mask = 0x55
		v = -v - 1
	}
	seg := 0
	for seg < 8 && v > aend[seg] {
		seg++
	}
	if seg >= 8 {
		return 0x7F ^ mask
	}
	aval := byte(seg << 4)
	if seg < 2 {
		aval |= byte((v >> 1) & 0x0F)
	} else {
		aval |= byte((v >> seg) & 0x0F)
	}
	return aval ^ mask
}

// aLawDecode converts one G.711 A-law byte to a 16-bit sample.
func aLawDecode(b byte) int16 {
	a := b ^ 0x55
	t := int32(a&0x0F) << 4
	seg := (a & 0x70) >> 4
	switch seg {
	case 0:
		t += 8
	case 1:
		t += 0x108
	default:
		t += 0x108
		t <<= seg - 1
	}
	if a&0x80 != 0 {
		return int16(t)
	}
	return int16(-t)
}

// audioMovingAverage is a stateful boxcar low-pass over the last n samples, used before downsampling.
type audioMovingAverage struct {
	window []int64
	pos    int
	sum    int64
}

// newAudioMovingAverage returns a filter over n samples; its history starts at zero.
func newAudioMovingAverage(n int) *audioMovingAverage {
	return &audioMovingAverage{window: make([]int64, n)}
}

// push adds one sample and returns the average of the last n samples.
func (m *audioMovingAverage) push(s int16) int16 {
	m.sum += int64(s) - m.window[m.pos]
	m.window[m.pos] = int64(s)
	m.pos = (m.pos + 1) % len(m.window)
	return int16(m.sum / int64(len(m.window)))
}

// pcmStreamResampler resamples mono PCM16 between two rates with linear interpolation, keeping state across Process calls. Output n sits at input position n*from/to and is the linear interpolation of the two input samples around it; an output whose right neighbour has not arrived yet waits for the next call. When downsampling, a moving average over ceil(from/to) samples runs first.
type pcmStreamResampler struct {
	from, to int
	base     int64   // absolute input index of in[0]
	in       []int16 // input samples from base on that later outputs may still need
	next     int64   // absolute index of the next output sample
	lowpass  *audioMovingAverage
}

// newPCMStreamResampler builds a resampler from one rate to another.
func newPCMStreamResampler(from, to int) *pcmStreamResampler {
	r := &pcmStreamResampler{from: from, to: to}
	if to < from {
		r.lowpass = newAudioMovingAverage((from + to - 1) / to)
	}
	return r
}

// Process resamples one chunk and returns the outputs that are now complete.
func (r *pcmStreamResampler) Process(in []int16) []int16 {
	if r.lowpass != nil {
		filtered := make([]int16, len(in))
		for i, s := range in {
			filtered[i] = r.lowpass.push(s)
		}
		in = filtered
	}
	r.in = append(r.in, in...)
	from, to := int64(r.from), int64(r.to)
	end := r.base + int64(len(r.in))
	out := make([]int16, 0, len(in)*r.to/r.from+1)
	for {
		pos := r.next * from
		idx := pos / to
		if idx+1 >= end {
			break
		}
		a := int64(r.in[idx-r.base])
		b := int64(r.in[idx+1-r.base])
		num := pos % to
		out = append(out, int16(a+(b-a)*num/to))
		r.next++
	}
	nextIdx := (r.next * from) / to
	drop := nextIdx - r.base
	if drop > int64(len(r.in)) {
		drop = int64(len(r.in))
	}
	if drop > 0 {
		r.in = r.in[drop:]
		r.base += drop
	}
	return out
}
