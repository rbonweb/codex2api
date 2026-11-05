package proxy

import (
	"bytes"
	"math"
	"testing"
)

// clientAudioTestSine returns n mono samples of a sine at freq Hz sampled at rate Hz.
func clientAudioTestSine(n, rate int, freq, amp float64) []int16 {
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(math.Round(amp * math.Sin(2*math.Pi*freq*float64(i)/float64(rate))))
	}
	return out
}

// clientAudioTestPeak returns the largest absolute sample value.
func clientAudioTestPeak(s []int16) int {
	peak := 0
	for _, v := range s {
		a := int(v)
		if a < 0 {
			a = -a
		}
		if a > peak {
			peak = a
		}
	}
	return peak
}

func TestParseClientAudioFormat(t *testing.T) {
	cases := []struct {
		name  string
		typ   string
		rate  int
		want  clientAudioFormat
		field string
	}{
		{name: "pcm 24k", typ: "audio/pcm", rate: 24000, want: clientAudioFormat{Type: "audio/pcm", Rate: 24000}},
		{name: "pcm 16k", typ: "audio/pcm", rate: 16000, want: clientAudioFormat{Type: "audio/pcm", Rate: 16000}},
		{name: "pcmu 8k", typ: "audio/pcmu", rate: 8000, want: clientAudioFormat{Type: "audio/pcmu", Rate: 8000}},
		{name: "pcma 8k", typ: "audio/pcma", rate: 8000, want: clientAudioFormat{Type: "audio/pcma", Rate: 8000}},
		{name: "pcm rate 0 defaults to 24k", typ: "audio/pcm", rate: 0, want: clientAudioFormat{Type: "audio/pcm", Rate: 24000}},
		{name: "pcmu rate 0 defaults to 8k", typ: "audio/pcmu", rate: 0, want: clientAudioFormat{Type: "audio/pcmu", Rate: 8000}},
		{name: "pcma rate 0 defaults to 8k", typ: "audio/pcma", rate: 0, want: clientAudioFormat{Type: "audio/pcma", Rate: 8000}},
		{name: "pcm 8k rejected", typ: "audio/pcm", rate: 8000, field: "rate"},
		{name: "opus rejected", typ: "audio/opus", rate: 0, field: "type"},
		{name: "pcmu 16k rejected", typ: "audio/pcmu", rate: 16000, field: "rate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, field, err := parseClientAudioFormat(tc.typ, tc.rate)
			if tc.field != "" {
				if err == nil {
					t.Fatalf("expected an error for %s/%d", tc.typ, tc.rate)
				}
				if field != tc.field {
					t.Fatalf("field = %q, want %q", field, tc.field)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if field != "" {
				t.Fatalf("field = %q on success, want empty", field)
			}
			if got != tc.want {
				t.Fatalf("format = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestG711KnownValues(t *testing.T) {
	if got := muLawEncode(0); got != 0xFF {
		t.Fatalf("muLawEncode(0) = %#x, want 0xFF", got)
	}
	if got := aLawEncode(0); got != 0xD5 {
		t.Fatalf("aLawEncode(0) = %#x, want 0xD5", got)
	}
	if got := muLawDecode(0xFF); got != 0 {
		t.Fatalf("muLawDecode(0xFF) = %d, want 0", got)
	}
	if got := aLawDecode(0xD5); got != 8 {
		t.Fatalf("aLawDecode(0xD5) = %d, want 8", got)
	}
}

func TestG711RoundTrip(t *testing.T) {
	for c := 0; c < 256; c++ {
		b := byte(c)
		if got := aLawEncode(aLawDecode(b)); got != b {
			t.Errorf("A-law code %#x decodes to %d and re-encodes to %#x", b, aLawDecode(b), got)
		}
		want := b
		if b == 0x7F {
			want = 0xFF // negative zero re-encodes as positive zero
		}
		if got := muLawEncode(muLawDecode(b)); got != want {
			t.Errorf("mu-law code %#x decodes to %d and re-encodes to %#x, want %#x", b, muLawDecode(b), got, want)
		}
	}
	for s := -32768; s <= 32767; s++ {
		v := int16(s)
		mag := s
		if mag < 0 {
			mag = -mag
		}
		if mag <= 256 {
			continue
		}
		for name, dec := range map[string]int{
			"mu-law": int(muLawDecode(muLawEncode(v))),
			"A-law":  int(aLawDecode(aLawEncode(v))),
		} {
			errAbs := dec - s
			if errAbs < 0 {
				errAbs = -errAbs
			}
			if errAbs*16 >= mag {
				t.Fatalf("%s sample %d decodes to %d: relative error %.4f is not under 1/16", name, s, dec, float64(errAbs)/float64(mag))
			}
		}
	}
}

func TestPCMStreamResamplerSplitEqualsWhole(t *testing.T) {
	pairs := []struct{ from, to int }{{16000, 24000}, {24000, 16000}, {8000, 24000}, {24000, 8000}}
	for _, p := range pairs {
		in := clientAudioTestSine(1000, p.from, 440, 10000)
		whole := newPCMStreamResampler(p.from, p.to).Process(in)
		for _, k := range []int{1, 7, 160, 333} {
			// Two chunks split at offset k.
			r := newPCMStreamResampler(p.from, p.to)
			split := append(append([]int16(nil), r.Process(in[:k])...), r.Process(in[k:])...)
			if !equalInt16(split, whole) {
				t.Errorf("%d->%d split at %d differs from whole (%d vs %d samples)", p.from, p.to, k, len(split), len(whole))
			}
			// Chunks of size k.
			r = newPCMStreamResampler(p.from, p.to)
			var chunked []int16
			for i := 0; i < len(in); i += k {
				end := i + k
				if end > len(in) {
					end = len(in)
				}
				chunked = append(chunked, r.Process(in[i:end])...)
			}
			if !equalInt16(chunked, whole) {
				t.Errorf("%d->%d chunks of %d differ from whole (%d vs %d samples)", p.from, p.to, k, len(chunked), len(whole))
			}
		}
	}
}

func TestPCMStreamResamplerLength(t *testing.T) {
	pairs := []struct{ from, to int }{{16000, 24000}, {24000, 16000}, {8000, 24000}, {24000, 8000}}
	for _, p := range pairs {
		for _, n := range []int{1000, 1001, 333} {
			got := len(newPCMStreamResampler(p.from, p.to).Process(make([]int16, n)))
			want := float64(n*p.to) / float64(p.from)
			// The trailing input sample is held for interpolation, so up to ceil(to/from) outputs wait for the next call.
			held := float64((p.to + p.from - 1) / p.from)
			if diff := want - float64(got); diff < -1 || diff > held {
				t.Errorf("%d->%d with %d samples gave %d outputs, want %.2f (at most %.0f held)", p.from, p.to, n, got, want, held)
			}
		}
	}
}

func TestPCMStreamResamplerPreservesTone(t *testing.T) {
	const amp = 10000
	pairs := []struct{ from, to int }{{16000, 24000}, {24000, 16000}, {8000, 24000}, {24000, 8000}}
	for _, p := range pairs {
		in := clientAudioTestSine(p.from, p.from, 440, amp)
		out := newPCMStreamResampler(p.from, p.to).Process(in)
		peak := clientAudioTestPeak(out)
		if peak < amp*85/100 || peak > amp*115/100 {
			t.Errorf("%d->%d peak %d, want within 15%% of %d", p.from, p.to, peak, amp)
		}
	}
}

func TestClientAudioDecoderEncoderRoundTrip(t *testing.T) {
	const amp = 10000
	formats := []clientAudioFormat{
		{Type: "audio/pcm", Rate: 24000},
		{Type: "audio/pcm", Rate: 16000},
		{Type: "audio/pcmu", Rate: 8000},
		{Type: "audio/pcma", Rate: 8000},
	}
	src := encodePCM16LE(clientAudioTestSine(24000, 24000, 440, amp))
	for _, f := range formats {
		enc := newClientAudioEncoder(f).Encode(src)
		dec := newClientAudioDecoder(f).Decode(enc)
		if f == clientAudioFormatDefault {
			if !bytes.Equal(dec, src) {
				t.Errorf("%+v: 24 kHz PCM round trip is not exact", f)
			}
			continue
		}
		samples, err := decodePCM16LE(dec)
		if err != nil {
			t.Fatalf("%+v: decoded output is not PCM16: %v", f, err)
		}
		if d := len(samples) - 24000; d < -3 || d > 3 {
			t.Errorf("%+v: round trip gave %d samples, want about 24000", f, len(samples))
		}
		if peak := clientAudioTestPeak(samples); peak < amp*80/100 || peak > amp*120/100 {
			t.Errorf("%+v: round trip peak %d, want within 20%% of %d", f, peak, amp)
		}
	}
}

func TestClientAudioOddByteCarry(t *testing.T) {
	dec := newClientAudioDecoder(clientAudioFormatDefault)
	if got := dec.Decode([]byte{0x01}); len(got) != 0 {
		t.Fatalf("Decode of one byte returned %d bytes, want 0", len(got))
	}
	if got := dec.Decode([]byte{0x02, 0x03}); !bytes.Equal(got, []byte{0x01, 0x02}) {
		t.Fatalf("Decode after carry returned %v, want [1 2]", got)
	}
	if got := dec.Decode([]byte{0x04}); !bytes.Equal(got, []byte{0x03, 0x04}) {
		t.Fatalf("Decode of the carried byte returned %v, want [3 4]", got)
	}

	enc := newClientAudioEncoder(clientAudioFormatDefault)
	if got := enc.Encode([]byte{0x01}); len(got) != 0 {
		t.Fatalf("Encode of one byte returned %d bytes, want 0", len(got))
	}
	if got := enc.Encode([]byte{0x02, 0x03}); !bytes.Equal(got, []byte{0x01, 0x02}) {
		t.Fatalf("Encode after carry returned %v, want [1 2]", got)
	}
	if got := enc.Encode([]byte{0x04}); !bytes.Equal(got, []byte{0x03, 0x04}) {
		t.Fatalf("Encode of the carried byte returned %v, want [3 4]", got)
	}
}

// equalInt16 reports whether two sample slices hold the same values.
func equalInt16(a, b []int16) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
