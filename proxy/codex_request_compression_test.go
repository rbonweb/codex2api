package proxy

import (
	"bytes"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// withCompressionSetting temporarily rewrites the compression switch in the system settings and restores it automatically when the test ends.
func withCompressionSetting(t *testing.T, enabled bool) {
	t.Helper()
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	next := previous
	next.CodexRequestCompression = enabled
	ApplyRuntimeSettings(next)
}

func decodeZstd(t *testing.T, data []byte) []byte {
	t.Helper()
	decoder, err := zstd.NewReader(nil)
	if err != nil {
		t.Fatalf("zstd.NewReader: %v", err)
	}
	defer decoder.Close()
	plain, err := decoder.DecodeAll(data, nil)
	if err != nil {
		t.Fatalf("upstream decompression failed: %v", err)
	}
	return plain
}

func TestCompressCodexRequestBodyDefaultsToEnabled(t *testing.T) {
	// Compression is on by default: the real Codex CLI already compresses /responses requests to the ChatGPT backend,
	// defaulting to off would make the default look unlike a real client.
	if !DefaultRuntimeSettings().CodexRequestCompression {
		t.Fatal("DefaultRuntimeSettings did not enable compression by default")
	}

	withCompressionSetting(t, true)
	body := []byte(`{"model":"gpt-5.4","instructions":"","input":[{"role":"user","content":"hello"}]}`)

	got, encoding := CompressCodexRequestBody(body)
	if encoding != "zstd" {
		t.Fatalf("Content-Encoding = %q, want %q", encoding, "zstd")
	}
	if bytes.Equal(got, body) {
		t.Fatal("body was not compressed")
	}
	if plain := decodeZstd(t, got); !bytes.Equal(plain, body) {
		t.Fatalf("decompressed result = %q, want %q", plain, body)
	}
}

func TestCompressCodexRequestBodyRespectsSystemSetting(t *testing.T) {
	// Turning the switch off in the admin panel must immediately return to plaintext — this is the one-click rollback path when something goes wrong, and it needs no restart.
	withCompressionSetting(t, false)
	body := []byte(`{"model":"gpt-5.4"}`)

	got, encoding := CompressCodexRequestBody(body)
	if encoding != "" {
		t.Fatalf("system setting is off, but Content-Encoding=%q is still returned", encoding)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("body was rewritten = %q, want %q", got, body)
	}
}

func TestCompressCodexRequestBodyEnvOverridesSystemSetting(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4"}`)

	// env is the deployment-level escape hatch: when the DB is unreachable or the admin panel cannot be opened, it must still be possible to force the switch, so it takes precedence over the system setting.
	t.Run("env forced on", func(t *testing.T) {
		withCompressionSetting(t, false)
		t.Setenv("CODEX_REQUEST_COMPRESSION", "zstd")
		if _, encoding := CompressCodexRequestBody(body); encoding != "zstd" {
			t.Fatalf("env=zstd did not override the system setting, encoding=%q", encoding)
		}
	})

	t.Run("env forced off", func(t *testing.T) {
		withCompressionSetting(t, true)
		t.Setenv("CODEX_REQUEST_COMPRESSION", "off")
		if _, encoding := CompressCodexRequestBody(body); encoding != "" {
			t.Fatalf("env=off did not override the system setting, encoding=%q", encoding)
		}
	})
}

func TestCodexRequestCompressionUnknownEnvFallsBackToSetting(t *testing.T) {
	// Unrecognized values must not be treated as "off", otherwise a typo would silently override the admin panel configuration.
	for _, value := range []string{"", "gzip", "br", "nonsense"} {
		t.Run("env="+value, func(t *testing.T) {
			withCompressionSetting(t, true)
			t.Setenv("CODEX_REQUEST_COMPRESSION", value)
			if !codexRequestCompressionEnabled() {
				t.Fatalf("CODEX_REQUEST_COMPRESSION=%q overrode the enabled state of the system setting", value)
			}

			withCompressionSetting(t, false)
			if codexRequestCompressionEnabled() {
				t.Fatalf("CODEX_REQUEST_COMPRESSION=%q overrode the disabled state of the system setting", value)
			}
		})
	}
}

func TestCompressCodexRequestBodyEmptyBodyStaysPlaintext(t *testing.T) {
	withCompressionSetting(t, true)
	got, encoding := CompressCodexRequestBody(nil)
	if encoding != "" || got != nil {
		t.Fatalf("empty body was compressed: body=%v encoding=%q", got, encoding)
	}
}
