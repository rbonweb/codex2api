package proxy

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

// --- fixture construction -----------------------------------------------------------

func sseBody(events ...string) io.ReadCloser {
	var b strings.Builder
	for _, ev := range events {
		b.WriteString("data: ")
		b.WriteString(ev)
		b.WriteString("\n\n")
	}
	return io.NopCloser(strings.NewReader(b.String()))
}

// sseResponse builds a successful test SSE response containing the given events.
func sseResponse(events ...string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Body: sseBody(events...)}
}

// delayedSSEResponse returns a test SSE response that writes its events after the given delay.
func delayedSSEResponse(delay time.Duration, events ...string) *http.Response {
	reader, writer := io.Pipe()
	payload := sseBody(events...)
	go func() {
		defer writer.Close()
		defer payload.Close()
		time.Sleep(delay)
		_, _ = io.Copy(writer, payload)
	}()
	return &http.Response{StatusCode: http.StatusOK, Body: reader}
}

func evCreated() string {
	return `{"type":"response.created","sequence_number":0,"response":{"id":"resp_r1","created_at":1700000000,"status":"in_progress"}}`
}

func evReasoningAdded(seq, oi int) string {
	return fmt.Sprintf(`{"type":"response.output_item.added","sequence_number":%d,"output_index":%d,"item":{"id":"rs_%d","type":"reasoning"}}`, seq, oi, oi)
}

func evReasoningDelta(seq, oi int, text string) string {
	return fmt.Sprintf(`{"type":"response.reasoning_summary_text.delta","sequence_number":%d,"output_index":%d,"delta":%q}`, seq, oi, text)
}

func evReasoningDone(seq, oi int, encrypted string) string {
	enc := ""
	if encrypted != "" {
		enc = fmt.Sprintf(`,"encrypted_content":%q`, encrypted)
	}
	return fmt.Sprintf(`{"type":"response.output_item.done","sequence_number":%d,"output_index":%d,"item":{"id":"rs_%d","type":"reasoning","summary":[]%s}}`, seq, oi, oi, enc)
}

func evMessageAdded(seq, oi int) string {
	return fmt.Sprintf(`{"type":"response.output_item.added","sequence_number":%d,"output_index":%d,"item":{"id":"msg_%d","type":"message","role":"assistant"}}`, seq, oi, oi)
}

func evMessageDelta(seq, oi int, text string) string {
	return fmt.Sprintf(`{"type":"response.output_text.delta","sequence_number":%d,"output_index":%d,"delta":%q}`, seq, oi, text)
}

func evMessageDone(seq, oi int, text string) string {
	return fmt.Sprintf(`{"type":"response.output_item.done","sequence_number":%d,"output_index":%d,"item":{"id":"msg_%d","type":"message","role":"assistant","content":[{"type":"output_text","text":%q}]}}`, seq, oi, oi, text)
}

func evCompleted(seq, inputTokens, outputTokens, reasoningTokens int) string {
	return fmt.Sprintf(`{"type":"response.completed","sequence_number":%d,"response":{"id":"resp_r1","status":"completed","usage":{"input_tokens":%d,"output_tokens":%d,"total_tokens":%d,"input_tokens_details":{"cached_tokens":40},"output_tokens_details":{"reasoning_tokens":%d}}}}`, seq, inputTokens, outputTokens, inputTokens+outputTokens, reasoningTokens)
}

func evFailed(seq int) string {
	return fmt.Sprintf(`{"type":"response.failed","sequence_number":%d,"response":{"id":"resp_r1","status":"failed","error":{"code":"server_error","message":"boom"}}}`, seq)
}

// collectFold runs one fold with a collector and returns the downstream events and the result.
type foldCollector struct {
	events   [][]byte
	rounds   [][]byte // request bodies received by openRound
	nextResp []*http.Response
	nextErr  []error
}

func (c *foldCollector) fold(baseBody string, maxRounds int, first *http.Response) continueFoldResult {
	f := &continueFold{
		baseBody:  []byte(baseBody),
		maxRounds: maxRounds,
		forward: func(data []byte) bool {
			c.events = append(c.events, append([]byte(nil), data...))
			return true
		},
		observe: func([]byte) {},
		openRound: func(body []byte) (*http.Response, error) {
			c.rounds = append(c.rounds, append([]byte(nil), body...))
			i := len(c.rounds) - 1
			if i < len(c.nextErr) && c.nextErr[i] != nil {
				return nil, c.nextErr[i]
			}
			if i < len(c.nextResp) {
				return c.nextResp[i], nil
			}
			return nil, fmt.Errorf("unexpected continuation round %d", i+2)
		},
		clientGone: func() bool { return false },
	}
	return runContinueThinkingFold(first, f)
}

func (c *foldCollector) eventTypes() []string {
	types := make([]string, 0, len(c.events))
	for _, ev := range c.events {
		types = append(types, gjson.GetBytes(ev, "type").String())
	}
	return types
}

// assertMonotonicSeq checks that downstream sequence_number strictly increases and output_index never decreases.
func assertMonotonicSeq(t *testing.T, events [][]byte) {
	t.Helper()
	prevSeq := int64(-1)
	for i, ev := range events {
		seq := gjson.GetBytes(ev, "sequence_number")
		if !seq.Exists() {
			t.Fatalf("event %d is missing sequence_number: %s", i, ev)
		}
		if seq.Int() != prevSeq+1 {
			t.Fatalf("event %d sequence_number is not contiguous: got %d want %d", i, seq.Int(), prevSeq+1)
		}
		prevSeq = seq.Int()
	}
}

// --- cases -------------------------------------------------------------------

func TestIsReasoningTruncationTokens(t *testing.T) {
	cases := []struct {
		tokens int
		want   bool
	}{
		{0, false}, {2, false}, {515, false}, {516, true}, {517, false},
		{518, false}, {1033, false}, {1034, true}, {1035, false},
		{518*7 - 2, true}, {518 * 7, false}, {-2, false},
	}
	for _, c := range cases {
		if got := isReasoningTruncationTokens(c.tokens); got != c.want {
			t.Errorf("isReasoningTruncationTokens(%d) = %v, want %v", c.tokens, got, c.want)
		}
	}
}

const testBaseBody = `{"model":"gpt-5.2","stream":true,"include":["reasoning.encrypted_content"],"input":[{"role":"user","content":"hi"}]}`

func TestFoldSingleCleanRound(t *testing.T) {
	c := &foldCollector{}
	res := c.fold(testBaseBody, 8, sseResponse(
		evCreated(),
		evReasoningAdded(1, 0),
		evReasoningDelta(2, 0, "thinking"),
		evReasoningDone(3, 0, "enc-a"),
		evMessageAdded(4, 1),
		evMessageDelta(5, 1, "hello"),
		evMessageDone(6, 1, "hello"),
		evCompleted(7, 100, 700, 300),
	))

	if res.StopReason != continueStopClean || res.RoundsRun != 1 || !res.GotTerminal {
		t.Fatalf("unexpected result: %+v", res)
	}
	if len(c.rounds) != 0 {
		t.Fatalf("a clean round should not trigger continue-thinking, got %d rounds", len(c.rounds))
	}
	types := c.eventTypes()
	want := []string{
		"response.created",
		"response.output_item.added", // reasoning, forwarded in real time
		"response.reasoning_summary_text.delta",
		"response.output_item.done",
		"response.output_item.added", // message flush
		"response.output_text.delta",
		"response.output_item.done",
		"response.completed",
	}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("event types = %v, want %v", types, want)
	}
	assertMonotonicSeq(t, c.events)

	final := c.events[len(c.events)-1]
	if got := gjson.GetBytes(final, "response.id").String(); got != "resp_r1" {
		t.Errorf("terminal response.id = %q", got)
	}
	if n := gjson.GetBytes(final, "response.output.#").Int(); n != 2 {
		t.Errorf("terminal output items = %d, want 2", n)
	}
	if got := gjson.GetBytes(final, "response.usage.output_tokens").Int(); got != 700 {
		t.Errorf("usage.output_tokens = %d, want 700 (300 reasoning + 400 final)", got)
	}
}

func TestFoldTwoRounds(t *testing.T) {
	c := &foldCollector{
		nextResp: []*http.Response{sseResponse(
			evCreated(), // hidden-round lifecycle events should be swallowed
			evReasoningAdded(1, 0),
			evReasoningDone(2, 0, "enc-b"),
			evMessageAdded(3, 1),
			evMessageDelta(4, 1, "real answer"),
			evMessageDone(5, 1, "real answer"),
			evCompleted(6, 120, 900, 400),
		)},
	}
	res := c.fold(testBaseBody, 8, sseResponse(
		evCreated(),
		evReasoningAdded(1, 0),
		evReasoningDone(2, 0, "enc-a"),
		evMessageAdded(3, 1),
		evMessageDelta(4, 1, "truncated junk"),
		evMessageDone(5, 1, "truncated junk"),
		evCompleted(6, 100, 600, 516), // 516 = matches the fingerprint
	))

	if res.StopReason != continueStopClean || res.RoundsRun != 2 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if len(res.Rounds) != 2 || res.Rounds[0].Usage.ReasoningTokens != 516 || res.Rounds[1].Usage.ReasoningTokens != 400 {
		t.Fatalf("rounds stat is wrong: %+v", res.Rounds)
	}
	if res.FinalUsage == nil || res.FinalUsage.OutputTokens != 900 {
		t.Fatalf("FinalUsage should be the final round's real value: %+v", res.FinalUsage)
	}

	// continue-thinking request body assertions
	if len(c.rounds) != 1 {
		t.Fatalf("expected 1 continue-thinking round, got %d", len(c.rounds))
	}
	body := c.rounds[0]
	input := gjson.GetBytes(body, "input").Array()
	if len(input) != 3 { // original user message + reasoning with id stripped + marker
		t.Fatalf("continuation input len = %d, want 3: %s", len(input), body)
	}
	if input[1].Get("type").String() != "reasoning" || input[1].Get("id").Exists() {
		t.Errorf("replayed reasoning should have its id stripped: %s", input[1].Raw)
	}
	if input[1].Get("encrypted_content").String() != "enc-a" {
		t.Errorf("replayed reasoning should keep encrypted_content: %s", input[1].Raw)
	}
	marker := input[2]
	if marker.Get("phase").String() != "commentary" || !strings.Contains(marker.Get("content.0.text").String(), "Continue thinking") {
		t.Errorf("marker shape is wrong: %s", marker.Raw)
	}
	if gjson.GetBytes(body, "previous_response_id").Exists() {
		t.Errorf("continue-thinking body should not have previous_response_id")
	}

	// downstream events: round 1's truncated message should not appear
	for _, ev := range c.events {
		if strings.Contains(string(ev), "truncated junk") {
			t.Fatalf("tentative output of the truncated round leaked downstream: %s", ev)
		}
	}
	types := c.eventTypes()
	want := []string{
		"response.created",                                        // round 1 only
		"response.output_item.added", "response.output_item.done", // r1 reasoning
		"response.output_item.added", "response.output_item.done", // r2 reasoning
		"response.output_item.added", "response.output_text.delta", "response.output_item.done", // r2 message flush
		"response.completed",
	}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("event types = %v, want %v", types, want)
	}
	assertMonotonicSeq(t, c.events)

	// rebuilt usage: input=round 1, reasoning=516+400, output=916+500 (non-reasoning part of the final round)
	final := c.events[len(c.events)-1]
	usage := gjson.GetBytes(final, "response.usage")
	if usage.Get("input_tokens").Int() != 100 {
		t.Errorf("usage.input_tokens = %d, want 100 (round 1)", usage.Get("input_tokens").Int())
	}
	if usage.Get("output_tokens_details.reasoning_tokens").Int() != 916 {
		t.Errorf("reasoning_tokens = %d, want 916", usage.Get("output_tokens_details.reasoning_tokens").Int())
	}
	if usage.Get("output_tokens").Int() != 916+500 {
		t.Errorf("output_tokens = %d, want 1416", usage.Get("output_tokens").Int())
	}
	if usage.Get("input_tokens_details.cached_tokens").Int() != 40 {
		t.Errorf("cached_tokens = %d, want 40", usage.Get("input_tokens_details.cached_tokens").Int())
	}
	// output_index should be renumbered to 0,1,2 (r1 reasoning, r2 reasoning, r2 message)
	var ois []int64
	for _, ev := range c.events {
		if gjson.GetBytes(ev, "type").String() == "response.output_item.added" {
			ois = append(ois, gjson.GetBytes(ev, "output_index").Int())
		}
	}
	if fmt.Sprint(ois) != "[0 1 2]" {
		t.Errorf("output_index sequence = %v, want [0 1 2]", ois)
	}
}

// TestFoldTerminalOutputKeepsOnlyLastRoundEncryptedContent verifies the fix for issue #353:
// in the folded response.completed.output, only the last round's reasoning keeps
// encrypted_content; earlier rounds are stripped (to prevent N copies of account-bound encrypted payloads, sent back by the client,
// from overflowing the context window or being rejected in bulk across accounts). The replayTail used for same-round continuation is not affected.
func TestFoldTerminalOutputKeepsOnlyLastRoundEncryptedContent(t *testing.T) {
	c := &foldCollector{
		nextResp: []*http.Response{
			// round 2: still matches the fingerprint (516) → continue to round 3
			sseResponse(
				evCreated(),
				evReasoningAdded(1, 0),
				evReasoningDone(2, 0, "enc-r2"),
				evCompleted(3, 120, 600, 516),
			),
			// round 3: ends cleanly (400, no match) → last round
			sseResponse(
				evCreated(),
				evReasoningAdded(1, 0),
				evReasoningDone(2, 0, "enc-r3"),
				evMessageAdded(3, 1),
				evMessageDelta(4, 1, "final answer"),
				evMessageDone(5, 1, "final answer"),
				evCompleted(6, 140, 900, 400),
			),
		},
	}
	res := c.fold(testBaseBody, 8, sseResponse(
		evCreated(),
		evReasoningAdded(1, 0),
		evReasoningDone(2, 0, "enc-r1"),
		evCompleted(3, 100, 600, 516), // matches the fingerprint
	))
	if res.RoundsRun != 3 || res.StopReason != continueStopClean {
		t.Fatalf("unexpected result: RoundsRun=%d stop=%s", res.RoundsRun, res.StopReason)
	}

	final := c.events[len(c.events)-1]
	if gjson.GetBytes(final, "type").String() != "response.completed" {
		t.Fatalf("final event should be response.completed: %s", final)
	}
	output := gjson.GetBytes(final, "response.output").Array()

	var reasoningEnc []string
	for _, item := range output {
		if item.Get("type").String() == "reasoning" {
			reasoningEnc = append(reasoningEnc, item.Get("encrypted_content").String())
		}
	}
	if len(reasoningEnc) != 3 {
		t.Fatalf("output should contain 3 reasoning items, got %d: %s", len(reasoningEnc), final)
	}
	// the first two rounds are stripped, the last round is kept
	if reasoningEnc[0] != "" || reasoningEnc[1] != "" {
		t.Errorf("earlier rounds' reasoning should have encrypted_content stripped: got %q, %q", reasoningEnc[0], reasoningEnc[1])
	}
	if reasoningEnc[2] != "enc-r3" {
		t.Errorf("last round's reasoning should keep encrypted_content: got %q", reasoningEnc[2])
	}

	// replayTail (sent upstream for continuation) is not affected: the round 1 continuation body still carries enc-r1
	if got := gjson.GetBytes(c.rounds[0], "input.1.encrypted_content").String(); got != "enc-r1" {
		t.Errorf("continuation body 1 should keep enc-r1, got %q", got)
	}
}

// TestFoldTerminalOutputKeepAllEncryptedEscapeHatch verifies that the escape hatch restores the old behavior.
func TestFoldTerminalOutputKeepAllEncryptedEscapeHatch(t *testing.T) {
	t.Setenv("CODEX_CONTINUE_KEEP_ALL_ENCRYPTED", "1")
	c := &foldCollector{
		nextResp: []*http.Response{sseResponse(
			evCreated(),
			evReasoningAdded(1, 0),
			evReasoningDone(2, 0, "enc-b"),
			evMessageAdded(3, 1),
			evMessageDone(5, 1, "answer"),
			evCompleted(6, 120, 900, 400),
		)},
	}
	c.fold(testBaseBody, 8, sseResponse(
		evCreated(),
		evReasoningAdded(1, 0),
		evReasoningDone(2, 0, "enc-a"),
		evCompleted(3, 100, 600, 516),
	))
	final := c.events[len(c.events)-1]
	output := gjson.GetBytes(final, "response.output").Array()
	if output[0].Get("encrypted_content").String() != "enc-a" {
		t.Errorf("with the escape hatch on, earlier rounds should keep encrypted_content: %s", output[0].Raw)
	}
}

func TestFoldMaxRoundsGuard(t *testing.T) {
	c := &foldCollector{}
	res := c.fold(testBaseBody, 1, sseResponse( // maxRounds=1: no continue-thinking even when the fingerprint matches
		evCreated(),
		evReasoningAdded(1, 0),
		evReasoningDone(2, 0, "enc-a"),
		evMessageAdded(3, 1),
		evMessageDone(4, 1, "answer"),
		evCompleted(5, 100, 600, 516),
	))
	if res.StopReason != continueStopMaxRounds || len(c.rounds) != 0 {
		t.Fatalf("should stop because of max_rounds without sending a continuation: %+v rounds=%d", res, len(c.rounds))
	}
	// the current output should still be flushed to the client
	types := c.eventTypes()
	if types[len(types)-1] != "response.completed" || !strings.Contains(strings.Join(types, ","), "response.output_item.added") {
		t.Fatalf("when the guardrail stops, output should be flushed and a terminal state given: %v", types)
	}
}

func TestFoldNoEncryptedContentGuard(t *testing.T) {
	c := &foldCollector{}
	res := c.fold(testBaseBody, 8, sseResponse(
		evCreated(),
		evReasoningAdded(1, 0),
		evReasoningDone(2, 0, ""), // no encrypted_content
		evMessageAdded(3, 1),
		evMessageDone(4, 1, "answer"),
		evCompleted(5, 100, 600, 516),
	))
	if res.StopReason != continueStopNoEncrypted || len(c.rounds) != 0 {
		t.Fatalf("missing encrypted_content should not trigger continue-thinking: %+v", res)
	}
}

func TestFoldContinuationRoundRejected(t *testing.T) {
	c := &foldCollector{
		nextResp: []*http.Response{{
			StatusCode: http.StatusBadRequest,
			Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"bad request"}}`)),
		}},
	}
	res := c.fold(testBaseBody, 8, sseResponse(
		evCreated(),
		evReasoningAdded(1, 0),
		evReasoningDone(2, 0, "enc-a"),
		evCompleted(3, 100, 516, 516),
	))
	if res.StopReason != continueStopRoundError {
		t.Fatalf("StopReason = %q, want continuation_error", res.StopReason)
	}
	// a failed continue-thinking open-round is recorded in FailedContinuation (kept separate from the real successful Rounds, to avoid double billing).
	if len(res.Rounds) != 1 {
		t.Fatalf("Rounds should contain only 1 successful round, got %+v", res.Rounds)
	}
	if res.FailedContinuation == nil || res.FailedContinuation.StatusCode != http.StatusBadRequest {
		t.Fatalf("a failed continuation should be recorded in FailedContinuation: %+v", res.FailedContinuation)
	}
	if res.FinalUsage == nil || res.FinalUsage.ReasoningTokens != 516 {
		t.Fatalf("FinalUsage should be the real usage of the last successful round: %+v", res.FinalUsage)
	}
	if !res.GotTerminal {
		t.Fatalf("continuation failed and a terminal state was synthesized, GotTerminal should be true")
	}
	final := c.events[len(c.events)-1]
	if gjson.GetBytes(final, "type").String() != "response.incomplete" {
		t.Fatalf("a failed continuation should synthesize response.incomplete: %s", final)
	}
	if gjson.GetBytes(final, "response.incomplete_details.reason").String() != "upstream_error" {
		t.Fatalf("incomplete reason is wrong: %s", final)
	}
	assertMonotonicSeq(t, c.events)
}

func TestFoldFirstRoundContinuationFailsBillsRound1(t *testing.T) {
	// round 1 matches the fingerprint but the first continuation fails when opening: FinalUsage must keep round 1's real usage
	// (otherwise the whole round's cost is missed), and a terminal state is synthesized (otherwise the account would be wrongly penalized for a stream break).
	c := &foldCollector{
		nextErr: []error{fmt.Errorf("dial timeout")},
	}
	res := c.fold(testBaseBody, 8, sseResponse(
		evCreated(),
		evReasoningAdded(1, 0),
		evReasoningDone(2, 0, "enc-a"),
		evCompleted(3, 4000, 516, 516),
	))
	if res.RoundsRun != 1 || res.StopReason != continueStopRoundError {
		t.Fatalf("unexpected: RoundsRun=%d stop=%s", res.RoundsRun, res.StopReason)
	}
	if res.FinalUsage == nil || res.FinalUsage.InputTokens != 4000 || res.FinalUsage.ReasoningTokens != 516 {
		t.Fatalf("round 1's real usage should be kept in FinalUsage: %+v", res.FinalUsage)
	}
	if res.FailedContinuation == nil {
		t.Fatalf("a failed first continuation should be recorded in FailedContinuation")
	}
	if !res.GotTerminal {
		t.Fatalf("a terminal state was synthesized, GotTerminal should be true (to avoid misreading a stream break)")
	}
}

func TestFoldTwoRoundsThenOpenFailsNoDoubleCount(t *testing.T) {
	// r1 matches → r2 matches → opening r3 fails. Rounds should contain the two successful rounds r1 and r2,
	// FailedContinuation records the r3 failure separately; FinalUsage=r2, the wrap-up bills r2,
	// logContinueThinkingRounds bills r1 plus a failure placeholder, and never bills r2 twice.
	c := &foldCollector{
		nextResp: []*http.Response{sseResponse(
			evCreated(),
			evReasoningAdded(1, 0),
			evReasoningDone(2, 0, "enc-b"),
			evCompleted(3, 120, 900, 516), // r2 also matches the fingerprint
		)},
		nextErr: []error{nil, fmt.Errorf("r3 dial fail")}, // the 2nd openRound (opening r3) fails
	}
	res := c.fold(testBaseBody, 8, sseResponse(
		evCreated(),
		evReasoningAdded(1, 0),
		evReasoningDone(2, 0, "enc-a"),
		evCompleted(3, 100, 600, 516),
	))
	if res.StopReason != continueStopRoundError {
		t.Fatalf("StopReason = %q, want continuation_error", res.StopReason)
	}
	if len(res.Rounds) != 2 {
		t.Fatalf("Rounds should contain 2 successful rounds, got %d: %+v", len(res.Rounds), res.Rounds)
	}
	if res.FailedContinuation == nil {
		t.Fatalf("the r3 failure should be recorded in FailedContinuation")
	}
	if res.FinalUsage == nil || res.FinalUsage.OutputTokens != 900 {
		t.Fatalf("FinalUsage should be r2: %+v", res.FinalUsage)
	}
	// Billing check: the wrap-up bills FinalUsage (r2), and logContinueThinkingRounds bills Rounds[:len-1]=[r1] plus the failure placeholder.
	// assert that Rounds[:len-1] is exactly [r1] and excludes r2, to avoid double billing with the wrap-up.
	if res.Rounds[0].Usage == nil || res.Rounds[0].Usage.InputTokens != 100 {
		t.Fatalf("Rounds[0] should be r1: %+v", res.Rounds[0].Usage)
	}
}

func TestFoldFirstRoundEOFSilent(t *testing.T) {
	c := &foldCollector{}
	res := c.fold(testBaseBody, 8, sseResponse(
		evCreated(), // lifecycle events only, no terminal
	))
	if res.GotTerminal || res.StopReason != continueStopUpstreamEOF {
		t.Fatalf("round 1 EOF should stay silent (left to the upper-layer transparent retry): %+v", res)
	}
	for _, ev := range c.events {
		if gjson.GetBytes(ev, "type").String() == "response.incomplete" {
			t.Fatalf("round 1 EOF with no output should not synthesize incomplete: %s", ev)
		}
	}
}

func TestFoldFirstRoundEOFWithBufferedContentFlushes(t *testing.T) {
	// round 1 buffered message content and then EOF (no terminal): it should not be silently dropped,
	// the buffered content should be flushed and incomplete synthesized, so the client does not get an empty 200 stream.
	c := &foldCollector{}
	res := c.fold(testBaseBody, 8, sseResponse(
		evCreated(),
		evMessageAdded(1, 0),
		evMessageDelta(2, 0, "partial answer"),
		// no terminal
	))
	if !res.GotTerminal || res.StopReason != continueStopUpstreamEOF {
		t.Fatalf("EOF with buffered content should synthesize a terminal state: %+v", res)
	}
	joined := ""
	for _, ev := range c.events {
		joined += string(ev)
	}
	if !strings.Contains(joined, "partial answer") {
		t.Fatalf("buffered content should be flushed to the client, events=%v", c.eventTypes())
	}
	if c.eventTypes()[len(c.eventTypes())-1] != "response.incomplete" {
		t.Fatalf("last event should be incomplete: %v", c.eventTypes())
	}
}

func TestFoldContinuationRoundEOF(t *testing.T) {
	c := &foldCollector{
		nextResp: []*http.Response{sseResponse(
			evReasoningAdded(1, 0), // continuation round stream cut, no terminal
		)},
	}
	res := c.fold(testBaseBody, 8, sseResponse(
		evCreated(),
		evReasoningAdded(1, 0),
		evReasoningDone(2, 0, "enc-a"),
		evCompleted(3, 100, 516, 516),
	))
	if !res.GotTerminal || res.StopReason != continueStopUpstreamEOF {
		t.Fatalf("continuation round EOF should synthesize a terminal state: %+v", res)
	}
	final := c.events[len(c.events)-1]
	if gjson.GetBytes(final, "type").String() != "response.incomplete" ||
		gjson.GetBytes(final, "response.incomplete_details.reason").String() != "upstream_eof" {
		t.Fatalf("should synthesize incomplete (upstream_eof): %s", final)
	}
}

func TestFoldFirstRoundFailedPassthrough(t *testing.T) {
	failed := evFailed(1)
	c := &foldCollector{}
	res := c.fold(testBaseBody, 8, sseResponse(
		evCreated(),
		failed,
	))
	if res.StopReason != continueStopClean {
		t.Fatalf("unexpected result: %+v", res)
	}
	final := c.events[len(c.events)-1]
	if string(final) != failed {
		t.Fatalf("round 1 with no output: failed should pass through byte-for-byte:\ngot  %s\nwant %s", final, failed)
	}
}

func TestFoldForwardAbort(t *testing.T) {
	aborted := false
	f := &continueFold{
		baseBody:  []byte(testBaseBody),
		maxRounds: 8,
		forward: func(data []byte) bool {
			aborted = true
			return false // simulates an upper-layer abort, e.g. failed suppression before the first packet
		},
		observe:    func([]byte) {},
		openRound:  func([]byte) (*http.Response, error) { return nil, fmt.Errorf("unreachable") },
		clientGone: func() bool { return false },
	}
	res := runContinueThinkingFold(sseResponse(
		evCreated(),
		evReasoningAdded(1, 0),
		evCompleted(2, 100, 516, 516),
	), f)
	if !aborted || res.StopReason != continueStopForwardAbort {
		t.Fatalf("forward=false should abort immediately: %+v", res)
	}
}

func TestFoldClientGoneStopsContinuation(t *testing.T) {
	c := &foldCollector{}
	f := &continueFold{
		baseBody:  []byte(testBaseBody),
		maxRounds: 8,
		forward: func(data []byte) bool {
			c.events = append(c.events, append([]byte(nil), data...))
			return true
		},
		observe:    func([]byte) {},
		openRound:  func([]byte) (*http.Response, error) { t.Fatal("no round after client disconnect"); return nil, nil },
		clientGone: func() bool { return true },
	}
	res := runContinueThinkingFold(sseResponse(
		evCreated(),
		evReasoningAdded(1, 0),
		evReasoningDone(2, 0, "enc-a"),
		evCompleted(3, 100, 516, 516),
	), f)
	if res.StopReason != continueStopClientGone {
		t.Fatalf("StopReason = %q, want client_gone", res.StopReason)
	}
}

// --- hidden-round downstream keepalive (issue #458) --------------------------------------------

// keepaliveFold builds a fold with a keepalive callback: openRound is delayed by openDelay to simulate a slow continue-thinking round.
func keepaliveFold(t *testing.T, counter *atomic.Int32, keepaliveRet func() bool, openDelay time.Duration, round2 *http.Response) *continueFold {
	t.Helper()
	return &continueFold{
		baseBody:  []byte(testBaseBody),
		maxRounds: 8,
		forward:   func([]byte) bool { return true },
		observe:   func([]byte) {},
		openRound: func([]byte) (*http.Response, error) {
			time.Sleep(openDelay)
			return round2, nil
		},
		clientGone:        func() bool { return false },
		keepalive:         func() bool { counter.Add(1); return keepaliveRet() },
		keepaliveInterval: 10 * time.Millisecond,
	}
}

func fingerprintFirstRound() *http.Response {
	return sseResponse(
		evCreated(),
		evReasoningAdded(1, 0),
		evReasoningDone(2, 0, "enc-a"),
		evCompleted(3, 100, 516, 516), // 516 matches the fingerprint → enter a hidden continue-thinking round
	)
}

func cleanSecondRound() *http.Response {
	return sseResponse(
		evReasoningAdded(1, 0),
		evReasoningDone(2, 0, "enc-b"),
		evCompleted(3, 120, 900, 400),
	)
}

// TestFoldKeepaliveFiresDuringHiddenRoundAndStopsAfterFold verifies that keepalive fires while a hidden continue-thinking round is waiting,
// and that the callback stops once the whole fold has finished.
func TestFoldKeepaliveFiresDuringHiddenRoundAndStopsAfterFold(t *testing.T) {
	var counter atomic.Int32
	f := keepaliveFold(t, &counter, func() bool { return true }, 80*time.Millisecond, cleanSecondRound())

	res := runContinueThinkingFold(fingerprintFirstRound(), f)
	if res.StopReason != continueStopClean || res.RoundsRun != 2 {
		t.Fatalf("unexpected result: %+v", res)
	}
	after := counter.Load()
	if after < 1 {
		t.Fatalf("openRound delayed 80ms, interval 10ms, keepalive should fire at least once, got %d", after)
	}

	// when fold returns, the deferred join has already synchronized with the keepalive goroutine: no callback may run after that.
	time.Sleep(50 * time.Millisecond)
	if got := counter.Load(); got != after {
		t.Fatalf("keepalive still fired after fold returned: %d -> %d", after, got)
	}
}

// TestFoldKeepaliveFiresDuringCleanFirstRoundAndStops verifies that keepalive is sent during a silent first round,
// and that the keepalive goroutine stops after folding ends.
func TestFoldKeepaliveFiresDuringCleanFirstRoundAndStops(t *testing.T) {
	var counter atomic.Int32
	f := keepaliveFold(t, &counter, func() bool { return true }, 0, nil)
	f.openRound = func([]byte) (*http.Response, error) {
		t.Fatal("a non-matching fingerprint should not open a continuation round")
		return nil, nil
	}
	resp := delayedSSEResponse(30*time.Millisecond,
		evCreated(),
		evReasoningAdded(1, 0),
		evReasoningDone(2, 0, "enc-a"),
		evCompleted(3, 100, 600, 400), // 400 does not match the fingerprint
	)

	res := runContinueThinkingFold(resp, f)
	if res.StopReason != continueStopClean || res.RoundsRun != 1 {
		t.Fatalf("unexpected result: %+v", res)
	}
	after := counter.Load()
	if after < 1 {
		t.Fatalf("first round delayed 30ms, interval 10ms, keepalive should fire at least once, got %d", after)
	}
	time.Sleep(20 * time.Millisecond)
	if got := counter.Load(); got != after {
		t.Fatalf("keepalive still fired after fold returned: %d -> %d", after, got)
	}
}

// TestFoldKeepaliveDisabledAtZeroInterval verifies that no heartbeat starts when the keepalive interval is zero.
func TestFoldKeepaliveDisabledAtZeroInterval(t *testing.T) {
	var counter atomic.Int32
	f := keepaliveFold(t, &counter, func() bool { return true }, 0, nil)
	f.keepaliveInterval = 0
	res := runContinueThinkingFold(delayedSSEResponse(30*time.Millisecond,
		evCreated(),
		evCompleted(1, 100, 600, 400),
	), f)
	if res.StopReason != continueStopClean || res.RoundsRun != 1 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if got := counter.Load(); got != 0 {
		t.Fatalf("keepalive should not fire when the interval is 0, got %d", got)
	}
}

func TestFoldKeepaliveStopsWhenCallbackReportsDeadClient(t *testing.T) {
	var counter atomic.Int32
	f := keepaliveFold(t, &counter, func() bool { return false }, 100*time.Millisecond, cleanSecondRound())

	res := runContinueThinkingFold(fingerprintFirstRound(), f)
	if res.StopReason != continueStopClean {
		t.Fatalf("unexpected result: %+v", res)
	}
	// the callback returns false on its first call and the ticker stops: within openRound's 100ms, a 10ms interval could have fired about 10 times.
	if got := counter.Load(); got != 1 {
		t.Fatalf("keepalive should stop immediately after the callback returns false, got %d times", got)
	}
}
