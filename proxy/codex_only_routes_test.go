package proxy

import (
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRegisterRoutesCodexOnlySurface(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	(&Handler{}).RegisterRoutes(engine)

	registered := make(map[string]bool)
	for _, route := range engine.Routes() {
		registered[route.Method+" "+route.Path] = true
	}

	present := []string{
		"POST /v1/responses",
		"GET /v1/responses",
		"POST /v1/responses/compact",
		"POST /v1/responses/input_tokens",
		"POST /v1/chat/completions",
		"POST /v1/images/generations",
		"POST /v1/images/edits",
		"GET /v1/models",
		"GET /v1/realtime",
		"POST /v1/live",
		"GET /v1/live/:call_id",
		"POST /v1/alpha/search",
		"POST /responses",
		"GET /responses",
		"POST /responses/compact",
		"POST /responses/input_tokens",
		"POST /chat/completions",
		"POST /images/generations",
		"POST /images/edits",
		"GET /models",
		"GET /realtime",
		"POST /live",
		"GET /live/:call_id",
		"POST /alpha/search",
		"POST /backend-api/codex/responses",
		"GET /backend-api/codex/responses",
		"GET /backend-api/codex/models",
		"POST /backend-api/codex/alpha/search",
		"POST /backend-api/codex/realtime/calls",
		"POST /backend-api/codex/responses/*subpath",
	}
	for _, key := range present {
		if !registered[key] {
			t.Errorf("expected route %q to be registered", key)
		}
	}

	absent := []string{
		"POST /v1/messages",
		"POST /v1/messages/count_tokens",
		"POST /messages",
		"POST /messages/count_tokens",
		"POST /v1/videos/generations",
		"POST /v1/videos/edits",
		"POST /v1/videos/extensions",
		"GET /v1/videos/:request_id",
		"GET /v1/videos/:request_id/content",
		"POST /videos/generations",
		"POST /videos/edits",
		"POST /videos/extensions",
		"GET /videos/:request_id",
		"GET /videos/:request_id/content",
		"GET /v1beta/models",
		"GET /v1beta/models/*action",
		"POST /v1beta/models/*action",
	}
	for _, key := range absent {
		if registered[key] {
			t.Errorf("route %q must not be registered", key)
		}
	}
}
