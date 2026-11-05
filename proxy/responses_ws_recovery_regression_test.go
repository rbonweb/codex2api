package proxy

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestResponsesWSUpstreamAuthenticationFailureIsLaneScoped(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			started, release := make(chan string, 1), make(chan struct{})
			client := newResponsesWSMultiplexClient(t, func(ctx context.Context, body []byte) (*http.Response, error) {
				if strings.Contains(string(body), "reject-upstream") {
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"code":"invalid_api_key","message":"synthetic upstream credential rejected"}}`))}, nil
				}
				started <- "healthy"
				select {
				case <-release:
					return responsesWSMultiplexResponse("resp_healthy"), nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			})
			responsesWSMultiplexSend(t, client, "healthy", "healthy")
			responsesWSMultiplexWaitStart(t, started, "healthy")
			responsesWSMultiplexSend(t, client, "rejected", "reject-upstream")
			frame := responsesWSMultiplexRead(t, client)
			if gjson.GetBytes(frame, "type").String() != "error" || gjson.GetBytes(frame, "stream_id").String() != "rejected" {
				t.Fatalf("upstream authentication error lost its lane: %s", frame)
			}
			close(release)
			responsesWSMultiplexReadTerminal(t, client, "healthy", "resp_healthy")
		})
	}
}

