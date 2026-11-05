package proxy

import (
	"os"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// Fidelity of Codex request body compression.
//
// The real Codex CLI compresses /responses request bodies to the ChatGPT backend with zstd by default: the feature
// enable_request_compression is Stage::Stable + in codex-rs/features/src/lib.rs, with
// default_enabled=true, and responses_request_compression in codex-rs/core/src/client.rs
// always returns Compression::Zstd for "ChatGPT backend + OpenAI provider". The encoding point is in
// codex-rs/http-client/src/request.rs: zstd level 3, Content-Encoding: zstd,
// no minimum length threshold (even a small body is compressed), and Content-Type stays application/json.
//
// This gateway has always sent plaintext JSON outbound, so "originator reports a Codex-family client and the UA carries a new version number,
// yet the body is 100% plaintext" forms a distinguishing feature present on every request with zero noise: the upstream only needs to
// group statistics by content-encoding to separate the proxy traffic as a whole, which is more stable than any single identifying field.
//
// Enabled by default (system setting codex_request_compression, DB default true): real clients compress anyway,
// so off-by-default means by default not looking like a real client. The switch can be hot-updated in the admin panel; if something goes wrong it is switched off in one click, with no restart.
//
// It only applies to the HTTP path and is orthogonal to codex_force_websocket. WS uses permessage-deflate
// (the dialer's EnableCompression is already on), both can apply at the same time, and WS does not need to be turned off for compression.
//
// One known imperfect fidelity point: the real client uses libzstd, while this gateway uses klauspost/compress.
// Both produce valid zstd frames and the upstream decompresses them to the same result, but the frame headers (window descriptor,
// content-length field encoding width) are not necessarily byte-identical. This is much closer than "not compressing at all",
// but it is not enough to defeat a byte-level fingerprint aimed at the compressor implementation.

// codexRequestCompressionEnabled decides whether this request is compressed.
//
// When the environment variable CODEX_REQUEST_COMPRESSION is set explicitly, it takes precedence over the system setting: it is a deployment-level escape hatch,
// and it still works when the DB is unreachable, the admin panel cannot be opened, or the whole machine must be forced back. When unset (or the value is not recognized),
// the system setting applies, controlled by the admin panel.
func codexRequestCompressionEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CODEX_REQUEST_COMPRESSION"))) {
	case "zstd", "on", "true", "1":
		return true
	case "off", "none", "false", "0", "plain":
		return false
	}
	return CurrentRuntimeSettings().CodexRequestCompression
}

var (
	codexZstdEncoderOnce sync.Once
	codexZstdEncoder     *zstd.Encoder
)

// codexRequestZstdEncoder returns a process-wide shared encoder. zstd.Encoder's EncodeAll is
// concurrency-safe, so there is no need to create one per request (that would allocate a full set of window buffers, a significant cost at high RPM).
// SpeedDefault corresponds to zstd level 3, matching the real client's encode_all(body, 3).
func codexRequestZstdEncoder() *zstd.Encoder {
	codexZstdEncoderOnce.Do(func() {
		encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
		if err != nil {
			// The parameters are constants, so construction failure is unreachable; leave nil so the caller falls back to plaintext instead of panicking.
			return
		}
		codexZstdEncoder = encoder
	})
	return codexZstdEncoder
}

// CompressCodexRequestBody compresses the request body for the official Codex upstream, according to the deployment tier.
//
// Returns (outbound bytes, Content-Encoding value). An empty second return value means not compressed, and the caller
// must not set the Content-Encoding header. Any compression failure falls back to plaintext: getting the request out matters more than a perfect shape.
//
// Only called on HTTP /responses. Neither of two adjacent paths should call it:
//   - /responses/compact: the real client takes this path through CompactClient::compact, which has no
//     compression parameter (codex-api/src/endpoint/compact.rs), and sends plaintext JSON.
//     Compressing both paths would look less like a real client than compressing neither.
//   - WebSocket: the payload is a response.create frame, compression semantics belong to the WS protocol layer, which is not the same as HTTP's
//     Content-Encoding.
func CompressCodexRequestBody(body []byte) ([]byte, string) {
	if len(body) == 0 || !codexRequestCompressionEnabled() {
		return body, ""
	}
	encoder := codexRequestZstdEncoder()
	if encoder == nil {
		return body, ""
	}
	compressed := encoder.EncodeAll(body, nil)
	if len(compressed) == 0 {
		return body, ""
	}
	return compressed, "zstd"
}
