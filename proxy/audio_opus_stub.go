//go:build !cgo || !opus

package proxy

// opusEncoder encodes 20 ms frames of 48 kHz mono PCM to Opus packets.
type opusEncoder interface {
	Encode(frame []int16) ([]byte, error)
	Close()
}

// newOpusEncoder is the stub for builds without libopus.
func newOpusEncoder() (opusEncoder, error) {
	return nil, errAudioBridgeUnavailable
}

// audioBridgeSupported reports whether this binary can encode Opus.
func audioBridgeSupported() bool {
	return false
}
