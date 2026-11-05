package proxy

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

// safeUpstreamHTTPStatus maps an upstream outcome status to a status that is safe to return downstream.
func safeUpstreamHTTPStatus(status int) int {
	if status < 400 || status > 599 || status == logStatusUpstreamStreamBreak || status == logStatusClientClosed {
		return http.StatusBadGateway
	}
	return status
}

// responsesNonStreamFailure classifies a non-stream Responses payload and reports whether it is a failure.
func responsesNonStreamFailure(payload []byte) (streamOutcome, bool) {
	if len(bytes.TrimSpace(payload)) == 0 || !gjson.ValidBytes(payload) {
		return streamOutcome{
			logStatusCode: logStatusUpstreamStreamBreak, failureKind: "transport",
			failureMessage: "Upstream returned an invalid JSON response", failurePayload: append([]byte(nil), payload...), penalize: true,
		}, true
	}
	root := gjson.ParseBytes(payload)
	status := strings.ToLower(strings.TrimSpace(root.Get("status").String()))
	failed := root.Get("type").String() == "response.failed" || status == "failed" ||
		(root.Get("error").Exists() && root.Get("error").Raw != "null") ||
		(status != "" && status != "completed" && status != "incomplete") ||
		(status == "" && root.Get("object").String() != "response")
	if !failed {
		return streamOutcome{}, false
	}
	return responsesStreamFailure(payload), true
}

func responsesStreamFailure(payload []byte) streamOutcome {
	outcome := classifyResponseFailedOutcome(payload)
	if outcome.failureMessage == "" || outcome.failureMessage == fmt.Sprintf("HTTP %d", outcome.logStatusCode) {
		outcome.failureMessage = "Upstream stream failed"
	}
	return outcome
}
