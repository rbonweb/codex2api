package proxy

import (
	"reflect"
	"testing"
)

func TestPCMRoundTrip(t *testing.T) {
	in := []int16{0, 1, -1, 32767, -32768, 1234}
	got, err := decodePCM16LE(encodePCM16LE(in))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("round trip = %v, want %v", got, in)
	}
	b := encodePCM16LE([]int16{0x0102})
	if !reflect.DeepEqual(b, []byte{0x02, 0x01}) {
		t.Fatalf("not little endian: %v", b)
	}
}

func TestPCMDecodeOddLengthErrors(t *testing.T) {
	if _, err := decodePCM16LE([]byte{1, 2, 3}); err == nil {
		t.Fatal("expected an error for odd length")
	}
	if got, err := decodePCM16LE(nil); err != nil || len(got) != 0 {
		t.Fatalf("empty input = %v, %v", got, err)
	}
}

func TestUpsamplerFirstSampleAndMidpoints(t *testing.T) {
	var u pcmUpsampler
	got := u.Process([]int16{100, 200, 300})
	want := []int16{100, 100, 150, 200, 250, 300}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("upsample = %v, want %v", got, want)
	}
}

func TestUpsamplerContinuityAcrossSplitChunks(t *testing.T) {
	in := make([]int16, 101)
	for i := range in {
		in[i] = int16((i*977)%20000 - 10000)
	}
	var whole pcmUpsampler
	want := whole.Process(in)
	for _, split := range []int{1, 2, 37, 100} {
		var u pcmUpsampler
		got := append(u.Process(in[:split]), u.Process(in[split:])...)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("split at %d differs from one-shot processing", split)
		}
	}
}

func TestUpsamplerReset(t *testing.T) {
	var u pcmUpsampler
	u.Process([]int16{1000})
	u.Reset()
	got := u.Process([]int16{50})
	if !reflect.DeepEqual(got, []int16{50, 50}) {
		t.Fatalf("after reset = %v", got)
	}
}

func TestResamplePCM16(t *testing.T) {
	in := make([]int16, 240)
	for i := range in {
		in[i] = int16(i * 10)
	}
	if got := resamplePCM16(in, 24000, 24000); !reflect.DeepEqual(got, in) {
		t.Fatal("equal rates must be the identity")
	}
	up := resamplePCM16(in, 24000, 48000)
	if len(up) != 480 {
		t.Fatalf("up length = %d, want 480", len(up))
	}
	if up[0] != 0 || up[1] != 5 || up[2] != 10 {
		t.Fatalf("up values = %v", up[:3])
	}
	down := resamplePCM16(in, 24000, 16000)
	if len(down) != 160 {
		t.Fatalf("down length = %d, want 160", len(down))
	}
	if down[0] != 0 || down[1] != 15 || down[2] != 30 {
		t.Fatalf("down values = %v", down[:3])
	}
	if got := resamplePCM16(nil, 24000, 48000); len(got) != 0 {
		t.Fatalf("empty input = %v", got)
	}
}

func TestFramerPushRemainderFlushReset(t *testing.T) {
	var f opusFramer
	if frames := f.Push(make([]int16, 500)); len(frames) != 0 || f.Pending() != 500 {
		t.Fatalf("frames=%d pending=%d", len(frames), f.Pending())
	}
	in := make([]int16, 1500)
	for i := range in {
		in[i] = int16(i + 1)
	}
	frames := f.Push(in)
	if len(frames) != 2 || f.Pending() != 80 {
		t.Fatalf("frames=%d pending=%d", len(frames), f.Pending())
	}
	for _, fr := range frames {
		if len(fr) != opusFrameSamples {
			t.Fatalf("frame length %d", len(fr))
		}
	}
	flushed := f.Flush()
	if len(flushed) != 1 || len(flushed[0]) != opusFrameSamples || f.Pending() != 0 {
		t.Fatalf("flush = %d frames, pending %d", len(flushed), f.Pending())
	}
	if flushed[0][0] != in[1420] || flushed[0][79] != in[1499] || flushed[0][80] != 0 {
		t.Fatalf("flush padding wrong: %d %d %d", flushed[0][0], flushed[0][79], flushed[0][80])
	}
	if f.Flush() != nil {
		t.Fatal("flush of an empty framer must return nothing")
	}
	f.Push(make([]int16, 10))
	f.Reset()
	if f.Pending() != 0 {
		t.Fatal("reset must drop pending samples")
	}
}

func TestFramerExactFrame(t *testing.T) {
	var f opusFramer
	frames := f.Push(make([]int16, opusFrameSamples))
	if len(frames) != 1 || f.Pending() != 0 {
		t.Fatalf("frames=%d pending=%d", len(frames), f.Pending())
	}
}
