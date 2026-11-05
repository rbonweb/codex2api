package proxy

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
)

// Upload limits for POST /v1/audio/transcriptions.
const (
	audioUploadMaxBytes   = 25 << 20
	audioUploadMaxSeconds = 120
)

// audioUploadSampleRate is the rate every upload is converted to before transcription.
const audioUploadSampleRate = 24000

var (
	errAudioUnsupportedFormat = errors.New("unsupported audio format; supported: wav (PCM 16-bit), pcm (raw 16-bit little-endian, 24 kHz mono)")
	errAudioTooLong           = errors.New("audio is longer than 120 seconds")
)

const (
	wavFormatPCM        = 1
	wavFormatExtensible = 0xFFFE
)

// decodeAudioUpload turns an uploaded audio file into 24 kHz mono PCM16 samples.
// WAV is detected by its RIFF header; raw PCM is accepted by file extension or content type.
// Every other format is rejected with errAudioUnsupportedFormat.
func decodeAudioUpload(filename, contentType string, data []byte) ([]int16, error) {
	if len(data) == 0 {
		return nil, errors.New("audio file is empty")
	}

	var (
		samples []int16
		err     error
	)
	switch {
	case looksLikeWAVUpload(data):
		samples, err = decodeWAVUpload(data)
	case isRawPCMUpload(filename, contentType):
		samples, err = decodeRawPCMUpload(data)
	default:
		return nil, errAudioUnsupportedFormat
	}
	if err != nil {
		return nil, err
	}
	if len(samples) == 0 {
		return nil, errors.New("audio file is empty")
	}
	if len(samples) > audioUploadMaxSeconds*audioUploadSampleRate {
		return nil, errAudioTooLong
	}
	return samples, nil
}

func looksLikeWAVUpload(data []byte) bool {
	if len(data) < 4 || string(data[:4]) != "RIFF" {
		return false
	}
	return len(data) < 12 || string(data[8:12]) == "WAVE"
}

func decodeWAVUpload(data []byte) ([]int16, error) {
	if len(data) < 12 {
		return nil, errors.New("malformed wav: truncated RIFF header")
	}

	var (
		fmtBody, pcm     []byte
		haveFmt, havePCM bool
	)
	pos := 12
	for pos+8 <= len(data) && !(haveFmt && havePCM) {
		id := string(data[pos : pos+4])
		size := uint64(binary.LittleEndian.Uint32(data[pos+4 : pos+8]))
		body := pos + 8
		if size > uint64(len(data)-body) {
			return nil, fmt.Errorf("malformed wav: %q chunk claims %d bytes but only %d remain", id, size, len(data)-body)
		}
		chunk := data[body : body+int(size)]
		switch id {
		case "fmt ":
			fmtBody, haveFmt = chunk, true
		case "data":
			pcm, havePCM = chunk, true
		}
		pos = body + int(size)
		if size%2 == 1 {
			pos++
		}
	}
	if !haveFmt {
		return nil, errors.New("malformed wav: no fmt chunk")
	}
	if !havePCM {
		return nil, errors.New("malformed wav: no data chunk")
	}
	if len(fmtBody) < 16 {
		return nil, errors.New("malformed wav: fmt chunk is shorter than 16 bytes")
	}

	format := binary.LittleEndian.Uint16(fmtBody[0:2])
	channels := int(binary.LittleEndian.Uint16(fmtBody[2:4]))
	rate := int(binary.LittleEndian.Uint32(fmtBody[4:8]))
	bits := binary.LittleEndian.Uint16(fmtBody[14:16])
	if format == wavFormatExtensible {
		if len(fmtBody) < 40 {
			return nil, errors.New("malformed wav: extensible fmt chunk is shorter than 40 bytes")
		}
		format = binary.LittleEndian.Uint16(fmtBody[24:26])
	}

	if format != wavFormatPCM {
		return nil, fmt.Errorf("%w (wav format tag %d is not PCM)", errAudioUnsupportedFormat, format)
	}
	if bits != 16 {
		return nil, fmt.Errorf("%w (wav has %d-bit samples)", errAudioUnsupportedFormat, bits)
	}
	if channels < 1 {
		return nil, errors.New("malformed wav: zero channels")
	}
	if rate < 8000 || rate > 96000 {
		return nil, fmt.Errorf("%w (wav sample rate %d Hz is outside 8000-96000 Hz)", errAudioUnsupportedFormat, rate)
	}

	frameBytes := 2 * channels
	frames := len(pcm) / frameBytes
	mono := make([]int16, frames)
	for f := 0; f < frames; f++ {
		sum := 0
		for c := 0; c < channels; c++ {
			offset := f*frameBytes + 2*c
			sum += int(int16(binary.LittleEndian.Uint16(pcm[offset : offset+2])))
		}
		mono[f] = int16(sum / channels)
	}

	if rate != audioUploadSampleRate {
		mono = resampleLinearInt16(mono, rate, audioUploadSampleRate)
	}
	return mono, nil
}

func isRawPCMUpload(filename, contentType string) bool {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".pcm", ".raw":
		return true
	}
	mediaType := strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	switch mediaType {
	case "audio/pcm", "audio/l16", "audio/x-pcm":
		return true
	}
	return false
}

func decodeRawPCMUpload(data []byte) ([]int16, error) {
	if len(data)%2 != 0 {
		return nil, fmt.Errorf("malformed raw pcm: %d bytes is not a whole number of 16-bit samples", len(data))
	}
	samples := make([]int16, len(data)/2)
	for i := range samples {
		samples[i] = int16(binary.LittleEndian.Uint16(data[2*i : 2*i+2]))
	}
	return samples, nil
}

// resampleLinearInt16 converts mono samples from one rate to another by linear interpolation.
func resampleLinearInt16(in []int16, from, to int) []int16 {
	if from == to || len(in) == 0 {
		return in
	}
	n := len(in) * to / from
	out := make([]int16, n)
	ratio := float64(from) / float64(to)
	for i := range out {
		pos := float64(i) * ratio
		idx := int(pos)
		if idx >= len(in) {
			idx = len(in) - 1
		}
		a := float64(in[idx])
		b := a
		if idx+1 < len(in) {
			b = float64(in[idx+1])
		}
		out[i] = int16(math.Round(a + (b-a)*(pos-float64(idx))))
	}
	return out
}
