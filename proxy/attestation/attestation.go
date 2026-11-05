// Package attestation unifies proof generation and failure state for the Codex desktop protocol.
package attestation

import "context"

const (
	HeaderName = "X-Oai-Attestation"
	BundleID   = "com.openai.codex"
	Version    = 1

	PlatformWindows = "win32"
	PlatformMacOS   = "darwin"

	UnsupportedPlatform     = 1
	UnsupportedArchitecture = 2
	UnsupportedDevice       = 3
	GenerationFailed        = 4
)

// Signals is the account device profile shared by all platforms and does not include real hardware attestation.
type Signals struct {
	Languages     []string
	Locale        string
	Timezone      string
	ScreenSizeSum uint64
	ScreenScale   float64
	SessionID     string
}

type Request struct {
	Platform string
	Arch     string
	Signals  Signals
}

// NativeResult keeps the difference between native supported and a missing token, corresponding to error codes 3 and 4.
type NativeResult struct {
	Supported   bool     `json:"supported"`
	TokenBase64 *string  `json:"tokenBase64,omitempty"`
	LatencyMS   *float64 `json:"latencyMs,omitempty"`
}

type NativeGenerator func(context.Context) (NativeResult, error)

type Service struct {
	native NativeGenerator
}

func NewService(native NativeGenerator) *Service {
	if native == nil {
		native = generateNative
	}
	return &Service{native: native}
}

type tokenResult struct {
	token     *string
	errorCode uint64
	latencyMS *float64
}

// Generate follows the outbound platform; on Linux and other platforms automatic attestation is disabled by default.
func (s *Service) Generate(ctx context.Context, request Request) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var result tokenResult
	switch request.Platform {
	case PlatformWindows:
		result.errorCode = UnsupportedPlatform
	case PlatformMacOS:
		result = s.generateMacOS(ctx, request.Arch)
	default:
		return "", nil
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return encodeEnvelope(result, request.Signals), nil
}

func (s *Service) generateMacOS(ctx context.Context, arch string) tokenResult {
	if arch != "arm64" {
		return tokenResult{errorCode: UnsupportedArchitecture}
	}
	native, err := s.native(ctx)
	if err != nil {
		return tokenResult{errorCode: GenerationFailed}
	}
	result := tokenResult{token: native.TokenBase64, latencyMS: native.LatencyMS}
	switch {
	case !native.Supported:
		result.token, result.errorCode = nil, UnsupportedDevice
	case native.TokenBase64 == nil:
		result.errorCode = GenerationFailed
	}
	return result
}
