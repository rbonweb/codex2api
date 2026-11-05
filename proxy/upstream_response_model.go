package proxy

import (
	"log"
	"strings"

	"github.com/tidwall/gjson"

	"github.com/codex2api/database"
)

// Upstream response model observation (ported from the upstream response model audit in sub2api):
// It records the model name the upstream response self-reports within one forwarding attempt, compares it with the model actually sent upstream, and writes it to the usage log,
// for the admin panel's "model mismatch" audit and filtering. Observation never affects the forwarding or billing path.
//
//   - first: the first declaration (such as response.model carried by a response.created frame)
//   - terminal: the terminal event declaration (response.completed / response.incomplete / response.failed),
//     takes precedence when present
//   - conflict: contradictory self-reports within the same attempt: a signal that upstream mixed streams or switched models midway
type upstreamResponseModelObserver struct {
	first    string
	terminal string
	conflict bool
}

// upstreamResponseModelMaxLength matches the column width of usage_logs.upstream_response_model.
const upstreamResponseModelMaxLength = 200

func (o *upstreamResponseModelObserver) Observe(model string, terminal bool) {
	model = normalizeObservedUpstreamResponseModel(model)
	if model == "" {
		return
	}
	if current := o.Model(); current != "" && !strings.EqualFold(current, model) {
		o.conflict = true
	}
	if terminal {
		o.terminal = model
		return
	}
	if o.first == "" {
		o.first = model
	}
}

// Model returns the upstream self-reported model observed for this attempt: the terminal declaration takes precedence, otherwise the first declaration;
// it is an empty string when upstream never self-reported.
func (o *upstreamResponseModelObserver) Model() string {
	if o == nil {
		return ""
	}
	if o.terminal != "" {
		return o.terminal
	}
	return o.first
}

func (o *upstreamResponseModelObserver) Conflict() bool {
	return o != nil && o.conflict
}

func normalizeObservedUpstreamResponseModel(model string) string {
	model = strings.TrimSpace(model)
	runes := []rune(model)
	if len(runes) > upstreamResponseModelMaxLength {
		model = string(runes[:upstreamResponseModelMaxLength])
	}
	return model
}

// observeUpstreamResponseModelFrame is called frame by frame by the SSE frame loop: if the frame body's response.model
// is non-empty, it is recorded; a successful terminal state and response.failed are handled as terminal declarations (the terminal declaration takes precedence).
func observeUpstreamResponseModelFrame(o *upstreamResponseModelObserver, parsed gjson.Result, eventType string) {
	if o == nil {
		return
	}
	if model := parsed.Get("response.model").String(); model != "" {
		o.Observe(model, isResponsesSuccessTerminalEvent(eventType) || eventType == "response.failed")
	}
}

// observeUpstreamResponseModelBody is for one-shot response bodies (non-streaming JSON / aggregated compact results)
// calls: the whole body is the terminal state.
func observeUpstreamResponseModelBody(o *upstreamResponseModelObserver, body []byte) {
	if o == nil || len(body) == 0 {
		return
	}
	o.Observe(gjson.GetBytes(body, "model").String(), true)
}

// upstreamModelMismatch has three states: if upstream did not self-report → nil (excluded from filtering and display; historical rows are likewise
// NULL); if it self-reported → strictly compared with the model actually sent, case-insensitively. Loose variant-level normalization (-latest /
// date suffix) is not done here: that is the display semantics of the frontend badge tiers; the stored value keeps the raw comparison result.
func upstreamModelMismatch(sentModel, responseModel string) *bool {
	responseModel = strings.TrimSpace(responseModel)
	if responseModel == "" {
		return nil
	}
	sentModel = strings.TrimSpace(sentModel)
	mismatch := sentModel == "" || !strings.EqualFold(sentModel, responseModel)
	return &mismatch
}

// upstreamSentModelForAudit returns the model actually sent, used for the audit comparison: prefers the attempt's actual sent model
// (an account-level mapping may rewrite it), falling back to the client's requested model.
func upstreamSentModelForAudit(attemptModel, logModel string) string {
	if m := strings.TrimSpace(attemptModel); m != "" {
		return m
	}
	return strings.TrimSpace(logModel)
}

// applyUpstreamResponseModelObservation writes the observation result into the usage log input. When upstream did not self-report,
// both fields stay at zero value/nil. A conflict logs one alert (aligned with sub2api's
// upstream_response_model_conflict); the stored value is Model() with the terminal declaration taking precedence.
func applyUpstreamResponseModelObservation(input *database.UsageLogInput, o *upstreamResponseModelObserver, sentModel string, accountID int64) {
	if input == nil || o == nil {
		return
	}
	model := o.Model()
	if model == "" {
		return
	}
	if o.Conflict() {
		log.Printf("upstream_response_model_conflict (account %d, endpoint %s): sent_model=%s, selected_response_model=%s", accountID, input.Endpoint, sentModel, model)
	}
	input.UpstreamResponseModel = model
	input.UpstreamModelMismatch = upstreamModelMismatch(sentModel, model)
}
