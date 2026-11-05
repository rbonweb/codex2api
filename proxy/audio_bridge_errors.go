package proxy

import (
	"errors"
	"net/http"

	"github.com/codex2api/api"
)

// errAudioBridgeUnavailable is returned by the audio bridge when the binary was built without libopus.
var errAudioBridgeUnavailable = errors.New("real-time audio needs a build with libopus (go build -tags opus with CGO); this binary was built without it")

// audioBridgeUnavailableStatus is the HTTP status the bridge endpoints answer with on a build without libopus.
const audioBridgeUnavailableStatus = http.StatusNotImplemented

// audioBridgeUnavailableAPIError is the API error body for audioBridgeUnavailableStatus.
func audioBridgeUnavailableAPIError() *api.APIError {
	return api.NewAPIError(api.ErrorCode("audio_bridge_unavailable"), errAudioBridgeUnavailable.Error(), api.ErrorTypeServer)
}

// opusSilenceFrame is the 3-byte Opus packet for a 20 ms frame of silence, sent while no audio is queued.
var opusSilenceFrame = []byte{0xf8, 0xff, 0xfe}
