package proxy

import (
	"io"
	"net/http"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// readUpstreamRequestBody reads the request body the mock upstream received and decompresses it according to Content-Encoding.
//
// zstd compression of the request body is enabled by default (aligned with the real Codex CLI, see codex_request_compression.go),
// so the fake upstream in tests must decompress just like the real upstream, otherwise the assertion would check the compressed frame instead of the JSON.
//
// Use it rather than turning compression off in tests: turning it off would mean the whole HTTP path test bypasses the default configuration,
// and the rewrite logic on that path would never again be covered in its real shape.
//
// The function is idempotent: without Content-Encoding it returns the input unchanged. Large Grok OAuth request bodies
// also go through zstd, and the fake upstream likewise decompresses here before asserting on the JSON.
func readUpstreamRequestBody(r *http.Request) []byte {
	if r == nil || r.Body == nil {
		return nil
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil || len(raw) == 0 {
		return raw
	}
	if !strings.EqualFold(strings.TrimSpace(r.Header.Get("Content-Encoding")), "zstd") {
		return raw
	}
	decoder, err := zstd.NewReader(nil)
	if err != nil {
		return raw
	}
	defer decoder.Close()
	plain, err := decoder.DecodeAll(raw, nil)
	if err != nil {
		return raw
	}
	return plain
}
