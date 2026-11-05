package admin

import (
	"context"

	"github.com/codex2api/internal/imageupscale"
)

// The upscale implementation has moved down to internal/imageupscale, sharing one set of
// target-size parsing and local/external backend selection with the public Images API; the old in-package names are kept here as delegates to avoid a wide change.

func upscaleImageBytes(ctx context.Context, imageBytes []byte, scale, requestedSize string) ([]byte, string, string, error) {
	return imageupscale.Bytes(ctx, imageBytes, scale, requestedSize)
}

func imageUpscalerBackend() string {
	return imageupscale.Backend()
}

func imageUpscaleTargetDimensions(width, height, targetLongSide int, requestedSize string) (int, int, bool) {
	return imageupscale.TargetDimensions(width, height, targetLongSide, requestedSize)
}

func normalizeImageUpscalerContentType(value string) string {
	return imageupscale.NormalizeContentType(value)
}
