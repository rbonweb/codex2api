package proxy

import (
	"encoding/binary"
	"errors"
	"strings"
	"testing"
)

func pcm16LEBytes(samples []int16) []byte {
	out := make([]byte, 2*len(samples))
	for i, s := range samples {
		binary.LittleEndian.PutUint16(out[2*i:], uint16(s))
	}
	return out
}

func riffTestChunk(id string, body []byte) []byte {
	out := make([]byte, 8, 8+len(body)+1)
	copy(out, id)
	binary.LittleEndian.PutUint32(out[4:], uint32(len(body)))
	out = append(out, body...)
	if len(body)%2 == 1 {
		out = append(out, 0)
	}
	return out
}

func testWAVFmt(format uint16, channels, rate, bits int) []byte {
	fmtBody := make([]byte, 16)
	blockAlign := channels * bits / 8
	binary.LittleEndian.PutUint16(fmtBody[0:], format)
	binary.LittleEndian.PutUint16(fmtBody[2:], uint16(channels))
	binary.LittleEndian.PutUint32(fmtBody[4:], uint32(rate))
	binary.LittleEndian.PutUint32(fmtBody[8:], uint32(rate*blockAlign))
	binary.LittleEndian.PutUint16(fmtBody[12:], uint16(blockAlign))
	binary.LittleEndian.PutUint16(fmtBody[14:], uint16(bits))
	return fmtBody
}

func testExtensibleFmt(subformat uint16, channels, rate int) []byte {
	fmtBody := testWAVFmt(wavFormatExtensible, channels, rate, 16)
	ext := make([]byte, 24)
	binary.LittleEndian.PutUint16(ext[0:], 22)
	binary.LittleEndian.PutUint16(ext[2:], 16)
	binary.LittleEndian.PutUint32(ext[4:], 4)
	binary.LittleEndian.PutUint16(ext[8:], subformat)
	return append(fmtBody, ext...)
}

func testWAVWithFmt(fmtBody, payload []byte, extra ...[]byte) []byte {
	body := []byte("WAVE")
	body = append(body, riffTestChunk("fmt ", fmtBody)...)
	for _, chunk := range extra {
		body = append(body, chunk...)
	}
	body = append(body, riffTestChunk("data", payload)...)
	header := make([]byte, 8)
	copy(header, "RIFF")
	binary.LittleEndian.PutUint32(header[4:], uint32(len(body)))
	return append(header, body...)
}

func testWAV(channels, rate int, payload []byte, extra ...[]byte) []byte {
	return testWAVWithFmt(testWAVFmt(wavFormatPCM, channels, rate, 16), payload, extra...)
}

func assertAudioSamples(t *testing.T, got, want []int16) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d samples %v, want %d samples %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sample %d: got %d, want %d (got %v)", i, got[i], want[i], got)
		}
	}
}

func assertAudioSamplesNear(t *testing.T, got, want []int16, tolerance int) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d samples %v, want %d samples %v", len(got), got, len(want), want)
	}
	for i := range want {
		diff := int(got[i]) - int(want[i])
		if diff < -tolerance || diff > tolerance {
			t.Fatalf("sample %d: got %d, want %d +/- %d (got %v)", i, got[i], want[i], tolerance, got)
		}
	}
}

func TestAudioDecodeSentinelMessages(t *testing.T) {
	const unsupported = "unsupported audio format; supported: wav (PCM 16-bit), pcm (raw 16-bit little-endian, 24 kHz mono)"
	if errAudioUnsupportedFormat.Error() != unsupported {
		t.Fatalf("unsupported sentinel message = %q", errAudioUnsupportedFormat.Error())
	}
	if errAudioTooLong.Error() != "audio is longer than 120 seconds" {
		t.Fatalf("too long sentinel message = %q", errAudioTooLong.Error())
	}
}

func TestAudioDecodeWAVMono24kPassesThrough(t *testing.T) {
	want := []int16{0, 1000, -1000, 32767, -32768}
	got, err := decodeAudioUpload("speech.wav", "audio/wav", testWAV(1, 24000, pcm16LEBytes(want)))
	if err != nil {
		t.Fatalf("decodeAudioUpload: %v", err)
	}
	assertAudioSamples(t, got, want)
}

func TestAudioDecodeWAVStereo48kDownmixesAndResamples(t *testing.T) {
	// Frames (L,R): (100,300) (0,0) (-200,-400) (1000,1000) average to 200, 0, -300, 1000.
	// Resampling 48 kHz to 24 kHz keeps every second mono frame: 200, -300.
	payload := pcm16LEBytes([]int16{100, 300, 0, 0, -200, -400, 1000, 1000})
	got, err := decodeAudioUpload("stereo.wav", "", testWAV(2, 48000, payload))
	if err != nil {
		t.Fatalf("decodeAudioUpload: %v", err)
	}
	assertAudioSamples(t, got, []int16{200, -300})
}

func TestAudioDecodeWAVMono16kResamplesUp(t *testing.T) {
	// 4 samples at 16 kHz become 6 samples at 24 kHz.
	payload := pcm16LEBytes([]int16{0, 300, 600, 900})
	got, err := decodeAudioUpload("narrow.wav", "", testWAV(1, 16000, payload))
	if err != nil {
		t.Fatalf("decodeAudioUpload: %v", err)
	}
	assertAudioSamplesNear(t, got, []int16{0, 200, 400, 600, 800, 900}, 1)
}

func TestAudioDecodeWAVSkipsChunksWithOddPadding(t *testing.T) {
	want := []int16{5, -5, 500}
	list := riffTestChunk("LIST", []byte("INFOabc"))
	junk := riffTestChunk("junk", []byte{9})
	got, err := decodeAudioUpload("tagged.wav", "", testWAV(1, 24000, pcm16LEBytes(want), list, junk))
	if err != nil {
		t.Fatalf("decodeAudioUpload: %v", err)
	}
	assertAudioSamples(t, got, want)
}

func TestAudioDecodeWAVExtensiblePCM(t *testing.T) {
	want := []int16{7, 8, 9}
	wav := testWAVWithFmt(testExtensibleFmt(wavFormatPCM, 1, 24000), pcm16LEBytes(want))
	got, err := decodeAudioUpload("ext.wav", "", wav)
	if err != nil {
		t.Fatalf("decodeAudioUpload: %v", err)
	}
	assertAudioSamples(t, got, want)
}

func TestAudioDecodeWAVRejectsUnsupportedSampleFormats(t *testing.T) {
	cases := map[string][]byte{
		"8-bit PCM":        testWAVWithFmt(testWAVFmt(wavFormatPCM, 1, 24000, 8), []byte{1, 2, 3}),
		"float32":          testWAVWithFmt(testWAVFmt(3, 1, 24000, 32), make([]byte, 8)),
		"extensible float": testWAVWithFmt(testExtensibleFmt(3, 1, 24000), make([]byte, 8)),
	}
	for name, wav := range cases {
		_, err := decodeAudioUpload("sample.wav", "", wav)
		if !errors.Is(err, errAudioUnsupportedFormat) {
			t.Fatalf("%s: err = %v, want errAudioUnsupportedFormat", name, err)
		}
		if !strings.Contains(err.Error(), "wav") {
			t.Fatalf("%s: error %q does not mention wav", name, err)
		}
	}
}

func TestAudioDecodeWAVRejectsOutOfRangeRate(t *testing.T) {
	for _, rate := range []int{4000, 192000} {
		_, err := decodeAudioUpload("rate.wav", "", testWAV(1, rate, pcm16LEBytes([]int16{1, 2})))
		if !errors.Is(err, errAudioUnsupportedFormat) {
			t.Fatalf("rate %d: err = %v, want errAudioUnsupportedFormat", rate, err)
		}
	}
}

func TestAudioDecodeWAVAcceptsEdgeRates(t *testing.T) {
	got, err := decodeAudioUpload("low.wav", "", testWAV(1, 8000, pcm16LEBytes([]int16{100, 200})))
	if err != nil {
		t.Fatalf("8 kHz: %v", err)
	}
	if len(got) != 6 || got[0] != 100 || got[5] != 200 {
		t.Fatalf("8 kHz: got %v, want 6 samples from 100 to 200", got)
	}

	got, err = decodeAudioUpload("high.wav", "", testWAV(1, 96000, pcm16LEBytes([]int16{0, 1, 2, 3, 4, 5, 6, 7})))
	if err != nil {
		t.Fatalf("96 kHz: %v", err)
	}
	assertAudioSamples(t, got, []int16{0, 4})
}

func TestAudioDecodeWAVMalformedIsRejected(t *testing.T) {
	full := testWAV(1, 24000, pcm16LEBytes([]int16{1, 2, 3, 4}))
	cases := map[string][]byte{
		"RIFF header cut short":           full[:6],
		"fmt chunk cut mid-body":          full[:20],
		"data chunk shorter than claimed": full[:len(full)-4],
		"zero channels":                   testWAVWithFmt(testWAVFmt(wavFormatPCM, 0, 24000, 16), pcm16LEBytes([]int16{1, 2})),
	}
	for name, wav := range cases {
		_, err := decodeAudioUpload("broken.wav", "", wav)
		if err == nil {
			t.Fatalf("%s: expected an error", name)
		}
		if !strings.Contains(err.Error(), "malformed wav") {
			t.Fatalf("%s: error %q does not say malformed wav", name, err)
		}
	}
}

func TestAudioDecodeRawPCMByName(t *testing.T) {
	want := []int16{1, -2, 300}
	for _, name := range []string{"clip.pcm", "CLIP.PCM", "clip.raw"} {
		got, err := decodeAudioUpload(name, "", pcm16LEBytes(want))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		assertAudioSamples(t, got, want)
	}
}

func TestAudioDecodeRawPCMByContentType(t *testing.T) {
	want := []int16{4, 5, -6}
	for _, contentType := range []string{"audio/pcm", "audio/L16; rate=24000", " Audio/X-PCM ", "audio/l16;channels=1"} {
		got, err := decodeAudioUpload("", contentType, pcm16LEBytes(want))
		if err != nil {
			t.Fatalf("%q: %v", contentType, err)
		}
		assertAudioSamples(t, got, want)
	}
}

func TestAudioDecodeRawPCMOddLengthIsRejected(t *testing.T) {
	_, err := decodeAudioUpload("odd.pcm", "", []byte{1, 2, 3})
	if err == nil {
		t.Fatal("expected an error for an odd byte count")
	}
	if errors.Is(err, errAudioUnsupportedFormat) {
		t.Fatalf("odd raw length should be a malformed-pcm error, got %v", err)
	}
	if !strings.Contains(err.Error(), "malformed raw pcm") {
		t.Fatalf("error %q does not say malformed raw pcm", err)
	}
}

func TestAudioDecodeUnsupportedFormatsAreRejected(t *testing.T) {
	webp := append([]byte("RIFF\x10\x00\x00\x00WEBP"), make([]byte, 8)...)
	cases := []struct {
		name, contentType string
		data              []byte
	}{
		{"speech.mp3", "audio/mpeg", []byte("ID3\x03\x00\x00\x00\x00\x00\x00")},
		{"speech.ogg", "", []byte("OggS\x00\x02")},
		{"speech.webm", "", []byte{0x1a, 0x45, 0xdf, 0xa3, 0x01}},
		{"speech.m4a", "", []byte("\x00\x00\x00\x20ftypM4A ")},
		{"speech.flac", "", []byte("fLaC\x00\x00\x00\x22")},
		{"image.webp", "", webp},
		{"noextension", "application/octet-stream", []byte{1, 2, 3, 4}},
	}
	for _, tc := range cases {
		_, err := decodeAudioUpload(tc.name, tc.contentType, tc.data)
		if !errors.Is(err, errAudioUnsupportedFormat) {
			t.Fatalf("%s: err = %v, want errAudioUnsupportedFormat", tc.name, err)
		}
	}
}

func TestAudioDecodeTooLong(t *testing.T) {
	limit := audioUploadMaxSeconds * audioUploadSampleRate

	if _, err := decodeAudioUpload("exact.pcm", "", pcm16LEBytes(make([]int16, limit))); err != nil {
		t.Fatalf("exactly %d seconds should be accepted: %v", audioUploadMaxSeconds, err)
	}

	_, err := decodeAudioUpload("long.pcm", "", pcm16LEBytes(make([]int16, limit+1)))
	if !errors.Is(err, errAudioTooLong) {
		t.Fatalf("raw over the limit: err = %v, want errAudioTooLong", err)
	}

	wav := testWAV(1, 16000, pcm16LEBytes(make([]int16, (audioUploadMaxSeconds+1)*16000)))
	_, err = decodeAudioUpload("long.wav", "", wav)
	if !errors.Is(err, errAudioTooLong) {
		t.Fatalf("wav over the limit: err = %v, want errAudioTooLong", err)
	}
}

func TestAudioDecodeEmptyIsRejected(t *testing.T) {
	for _, data := range [][]byte{nil, {}} {
		_, err := decodeAudioUpload("empty.wav", "audio/wav", data)
		if err == nil || err.Error() != "audio file is empty" {
			t.Fatalf("empty bytes: err = %v, want audio file is empty", err)
		}
	}

	_, err := decodeAudioUpload("empty-data.wav", "", testWAV(1, 24000, nil))
	if err == nil || err.Error() != "audio file is empty" {
		t.Fatalf("wav with an empty data chunk: err = %v, want audio file is empty", err)
	}
}
