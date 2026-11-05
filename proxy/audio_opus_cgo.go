//go:build cgo && opus

package proxy

/*
#cgo LDFLAGS: -lopus
#cgo darwin CFLAGS: -I/opt/homebrew/include
#cgo darwin LDFLAGS: -L/opt/homebrew/lib
#include <opus/opus.h>

// opus_encoder_ctl is variadic and cannot be called from Go, so wrap the bitrate setting.
static int codex2api_opus_set_bitrate(OpusEncoder *enc, opus_int32 bitrate) {
	return opus_encoder_ctl(enc, OPUS_SET_BITRATE(bitrate));
}
*/
import "C"

import (
	"errors"
	"fmt"
	"sync"
	"unsafe"
)

const (
	opusSampleRate  = 48000
	opusChannels    = 1
	opusBitrate     = 32000
	opusMaxPacketSz = 1500
)

// opusEncoder encodes 20 ms frames of 48 kHz mono PCM to Opus packets.
type opusEncoder interface {
	Encode(frame []int16) ([]byte, error)
	Close()
}

type cgoOpusEncoder struct {
	mu  sync.Mutex
	enc *C.OpusEncoder
	out [opusMaxPacketSz]byte
}

// newOpusEncoder creates a 48 kHz mono VOIP encoder at 32 kbit/s.
func newOpusEncoder() (opusEncoder, error) {
	var cerr C.int
	enc := C.opus_encoder_create(opusSampleRate, opusChannels, C.OPUS_APPLICATION_VOIP, &cerr)
	if cerr != C.OPUS_OK || enc == nil {
		return nil, fmt.Errorf("opus_encoder_create failed: %s", C.GoString(C.opus_strerror(cerr)))
	}
	if rc := C.codex2api_opus_set_bitrate(enc, opusBitrate); rc != C.OPUS_OK {
		C.opus_encoder_destroy(enc)
		return nil, fmt.Errorf("opus set bitrate failed: %s", C.GoString(C.opus_strerror(rc)))
	}
	return &cgoOpusEncoder{enc: enc}, nil
}

// Encode encodes exactly one 960-sample frame.
func (e *cgoOpusEncoder) Encode(frame []int16) ([]byte, error) {
	if len(frame) != opusFrameSamples {
		return nil, fmt.Errorf("opus frame must be %d samples, got %d", opusFrameSamples, len(frame))
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.enc == nil {
		return nil, errors.New("opus encoder is closed")
	}
	n := C.opus_encode(e.enc, (*C.opus_int16)(unsafe.Pointer(&frame[0])), C.int(opusFrameSamples), (*C.uchar)(unsafe.Pointer(&e.out[0])), C.opus_int32(len(e.out)))
	if n < 0 {
		return nil, fmt.Errorf("opus_encode failed: %s", C.GoString(C.opus_strerror(C.int(n))))
	}
	return append([]byte(nil), e.out[:int(n)]...), nil
}

// Close frees the encoder. It is safe to call more than once.
func (e *cgoOpusEncoder) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.enc != nil {
		C.opus_encoder_destroy(e.enc)
		e.enc = nil
	}
}

// audioBridgeSupported reports whether this binary can encode Opus.
func audioBridgeSupported() bool {
	return true
}
