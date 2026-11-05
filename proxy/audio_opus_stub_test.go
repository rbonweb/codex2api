//go:build !cgo || !opus

package proxy

import (
	"errors"
	"net/http"
	"testing"
)

func TestOpusStubIsUnavailable(t *testing.T) {
	enc, err := newOpusEncoder()
	if enc != nil || !errors.Is(err, errAudioBridgeUnavailable) {
		t.Fatalf("newOpusEncoder = %v, %v", enc, err)
	}
	if audioBridgeSupported() {
		t.Fatal("audioBridgeSupported must be false without libopus")
	}
}

func TestAudioBridgeUnavailableAPIError(t *testing.T) {
	apiErr := audioBridgeUnavailableAPIError()
	if string(apiErr.Code) != "audio_bridge_unavailable" {
		t.Fatalf("code = %q", apiErr.Code)
	}
	if audioBridgeUnavailableStatus != http.StatusNotImplemented {
		t.Fatalf("status = %d", audioBridgeUnavailableStatus)
	}
}
