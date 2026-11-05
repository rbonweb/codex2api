package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// This file implements automatic continue-thinking for truncated reasoning: when upstream truncates reasoning by a feature fingerprint
// (reasoning_tokens == 518*n - 2, e.g. 516, 1034), automatically carries this round's reasoning
// resends the request, and folds multiple upstream responses into one downstream SSE response.
//
// Folding rules:
//   - events for reasoning-type output items are forwarded in real time (sequence_number/output_index rewritten);
//   - events for message, function_call and other items are first buffered as "tentative output";
//   - once the terminal event arrives, the fingerprint is checked: on a match, tentative output is discarded and a continue-thinking round opens,
//     otherwise tentative output is flushed byte-for-byte and the terminal event is rebuilt.

const (
	reasoningTruncationStep    = 518
	continueThinkingMarkerText = "Continue thinking..."

	continueKeepaliveComment = ": keepalive\n\n"
)

// isReasoningTruncationTokens reports whether the reasoning token count matches the truncation fingerprint (518*n - 2).
func isReasoningTruncationTokens(tokens int) bool {
	return tokens >= reasoningTruncationStep-2 && (tokens+2)%reasoningTruncationStep == 0
}

// continueKeepAllEncrypted is an escape hatch: with CODEX_CONTINUE_KEEP_ALL_ENCRYPTED=1 it restores
// the old behavior of keeping every round's encrypted_content in the folded output (before issue #353).
func continueKeepAllEncrypted() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CODEX_CONTINUE_KEEP_ALL_ENCRYPTED"))) {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}

// stripReasoningEncryptedContent removes the encrypted_content field from reasoning items
// (keeps the summary text); non-reasoning items are returned as they are.
func stripReasoningEncryptedContent(item json.RawMessage) json.RawMessage {
	if gjson.GetBytes(item, "type").String() != "reasoning" {
		return item
	}
	if !gjson.GetBytes(item, "encrypted_content").Exists() {
		return item
	}
	if out, err := sjson.DeleteBytes([]byte(item), "encrypted_content"); err == nil {
		return out
	}
	return item
}

// Stop reasons for continue-thinking folds.
const (
	continueStopClean        = "clean"              // upstream ended cleanly (fingerprint not matched)
	continueStopMaxRounds    = "max_rounds"         // fingerprint still matched, but the maximum number of rounds was reached
	continueStopNoEncrypted  = "no_encrypted"       // fingerprint matched but reasoning lacks encrypted_content
	continueStopClientGone   = "client_gone"        // client disconnected, no more continue-thinking
	continueStopUpstreamEOF  = "upstream_eof"       // upstream closed without sending a terminal event
	continueStopRoundError   = "continuation_error" // continue-thinking round request failed (4xx or transport error)
	continueStopForwardAbort = "forward_abort"      // downstream callback requested abort (handed back to the upper-layer semantics)
)

// continueRoundStat records the real cost of one upstream request round, for per-round usage accounting.
type continueRoundStat struct {
	Trace      upstreamTraceSnapshot
	Usage      *UsageInfo
	StatusCode int
	DurationMs int
	ErrMessage string
}

// continueFoldResult is the summary result of one fold.
type continueFoldResult struct {
	// Rounds holds only real upstream rounds that successfully read a terminal event, in order; the last entry corresponds to FinalUsage.
	Rounds []continueRoundStat
	// FailedContinuation records failed continue-thinking open-round attempts (openRound error / non-200 / failed to build the body),
	// kept apart from Rounds so per-round accounting does not mix them with the final successful round and bill twice.
	FailedContinuation *continueRoundStat
	FinalUsage         *UsageInfo     // real usage of the final successful round (used for terminal billing)
	FinalResponse      *http.Response // final round response (body already closed, only used to read headers for cooldown/quota sync)
	GotTerminal        bool           // whether the client has already received the terminal event (decides whether the stream counts as a normal end)
	ReadErr            error
	StopReason         string
	RoundsRun          int
}

// continueFold holds the external dependencies and configuration of the fold state machine.
type continueFold struct {
	trace     func() upstreamTraceSnapshot
	baseBody  []byte // upstream request body actually sent for this attempt (already through the prepare pipeline)
	maxRounds int    // maximum number of rounds (including the first round)

	forward    func(data []byte) bool                    // existing downstream event callback; returns false to abort
	observe    func(data []byte)                         // side observation of buffered (not forwarded) events, e.g. TTFT
	openRound  func(body []byte) (*http.Response, error) // opens a continue-thinking round on the same account/channel
	clientGone func() bool                               // whether the client has disconnected

	// keepalive writes SSE comments downstream while folding; it returns false when the downstream is dead, and then stops
	// keepalive (folding itself continues and is wrapped up by the clientGone semantics). nil disables keepalive. The callback runs in a separate
	// goroutine, concurrent with forward; the caller is responsible for write-path mutual exclusion; before the first real byte is written
	// whether to skip is also left to the callback itself (an early write would commit the 200 header).
	keepalive func() bool
	// keepaliveInterval is the keepalive interval; <=0 disables it. For tests.
	keepaliveInterval time.Duration
}

func (f *continueFold) snapshotTrace() upstreamTraceSnapshot {
	if f.trace != nil {
		return f.trace()
	}
	return upstreamTraceSnapshot{}
}

// bufferedOutputItem buffers the complete event sequence of one non-reasoning output item.
type bufferedOutputItem struct {
	upstreamOI int64
	itemType   string
	events     [][]byte        // raw event bytes (not rewritten)
	item       json.RawMessage // latest item snapshot (carried by added or done)
}

// foldState is the cross-round internal state of the fold.
type foldState struct {
	seq  int64 // downstream sequence_number, monotonic counter
	dsOI int64 // downstream output_index, monotonic counter

	baseResponse json.RawMessage   // response object of round 1's response.created
	finalOutput  []json.RawMessage // finalized downstream output items (reasoning from each round + the final flushed items)
	replayTail   []json.RawMessage // items appended to input when continuing (reasoning with id stripped + marker)
	firstUsage   *UsageInfo        // round 1 usage (source of input/cached for rebuilding downstream usage)
	sumReasoning int               // sum of reasoning tokens across rounds
	flushedAny   bool              // whether tentative output has already been flushed to the client

	// lastRoundReasoningStart is the length of finalOutput when the last readRound started.
	// items before this index belong to earlier rounds' reasoning: when sent to the client, their encrypted_content is stripped,
	// only the last round keeps its full encrypted context (see clientFacingOutput / issue #353).
	lastRoundReasoningStart int
}

func (st *foldState) nextSeq() int64 {
	s := st.seq
	st.seq++
	return s
}

// rewriteEvent rewrites the event's sequence_number (and optionally output_index) and returns the new bytes.
func (st *foldState) rewriteEvent(data []byte, outputIndex int64, setOI bool) []byte {
	out, err := sjson.SetBytes(data, "sequence_number", st.nextSeq())
	if err != nil {
		out = data
	}
	if setOI && gjson.GetBytes(out, "output_index").Exists() {
		if rewritten, err := sjson.SetBytes(out, "output_index", outputIndex); err == nil {
			out = rewritten
		}
	}
	return out
}

// continueMarkerItem returns the continue-thinking marker: an assistant message with phase:"commentary"
// that prompts the model to keep thinking from the replayed reasoning, rather than treating the previous round as the final answer.
func continueMarkerItem() json.RawMessage {
	item := map[string]any{
		"type": "message",
		"role": "assistant",
		"content": []map[string]any{
			{"type": "output_text", "text": continueThinkingMarkerText},
		},
		"phase": "commentary",
	}
	raw, _ := json.Marshal(item)
	return raw
}

// buildContinuationBody replaces input with originalInput + replayTail in the original request body.
// baseBody has already gone through the prepare pipeline (stream=true, include carries encrypted_content, no
// previous_response_id), so here we only need to replace input.
func buildContinuationBody(baseBody []byte, replayTail []json.RawMessage) ([]byte, error) {
	origInput := gjson.GetBytes(baseBody, "input")
	items := make([]json.RawMessage, 0, 8+len(replayTail))
	if origInput.IsArray() {
		origInput.ForEach(func(_, v gjson.Result) bool {
			items = append(items, json.RawMessage(v.Raw))
			return true
		})
	}
	items = append(items, replayTail...)
	merged, err := json.Marshal(items)
	if err != nil {
		return nil, err
	}
	return sjson.SetRawBytes(baseBody, "input", merged)
}

// clientFacingOutput returns the output items sent to the client: only the last round's reasoning
// keeps its full encrypted_content, and earlier rounds' reasoning has that field stripped. Continue-thinking accumulates each round's
// reasoning in full into finalOutput (a normal single response contains just one copy); without convergence the client would send back
// N copies of account-bound encrypted payloads, growing the context past the window round by round and, when switching accounts, being rejected in bulk
// (issue #353). The encrypted context of earlier rounds has already been replayed via replayTail and produced the final answer during folding,
// so its job is done and the client does not need to drive it again. See continueKeepAllEncrypted for the escape hatch.
func (st *foldState) clientFacingOutput() []json.RawMessage {
	start := st.lastRoundReasoningStart
	if continueKeepAllEncrypted() || start <= 0 || start > len(st.finalOutput) {
		return st.finalOutput
	}
	out := make([]json.RawMessage, len(st.finalOutput))
	for i, item := range st.finalOutput {
		if i < start {
			out[i] = stripReasoningEncryptedContent(item)
		} else {
			out[i] = item
		}
	}
	return out
}

// rebuildTerminalEvent rebuilds the downstream terminal event after folding: it keeps round 1's response
// identity (id/created_at), takes the final round's status/error/incomplete_details, output
// is replaced with the full folded list, and usage is replaced with a rebuilt single-response view.
func (st *foldState) rebuildTerminalEvent(terminal []byte, finalRoundUsage *UsageInfo) []byte {
	base := st.baseResponse
	if len(base) == 0 {
		base = json.RawMessage(`{}`)
	}
	resp := []byte(base)
	tresp := gjson.GetBytes(terminal, "response")
	for _, key := range []string{"status", "status_code", "error", "incomplete_details", "service_tier"} {
		if v := tresp.Get(key); v.Exists() {
			resp, _ = sjson.SetRawBytes(resp, key, []byte(v.Raw))
		}
	}
	outputRaw, err := json.Marshal(st.clientFacingOutput())
	if err != nil {
		outputRaw = []byte(`[]`)
	}
	resp, _ = sjson.SetRawBytes(resp, "output", outputRaw)
	if usageRaw := st.rebuildUsage(finalRoundUsage); usageRaw != nil {
		resp, _ = sjson.SetRawBytes(resp, "usage", usageRaw)
	}

	eventType := gjson.GetBytes(terminal, "type").String()
	if eventType == "" {
		eventType = "response.completed"
	}
	out := []byte(`{}`)
	out, _ = sjson.SetBytes(out, "type", eventType)
	out, _ = sjson.SetRawBytes(out, "response", resp)
	out, _ = sjson.SetBytes(out, "sequence_number", st.nextSeq())
	return out
}

// rebuildUsage rebuilds multi-round usage into a single-response view: input/cached come from round 1 (what the client actually received),
// reasoning is summed across rounds (all of it was forwarded to the client), output = sum of reasoning
// plus the non-reasoning part of the final flushed round. The hidden rounds' input is not summed, to avoid the client wrongly concluding
// the context has overflowed the window.
func (st *foldState) rebuildUsage(finalRoundUsage *UsageInfo) []byte {
	if st.firstUsage == nil {
		return nil
	}
	inputTokens := st.firstUsage.InputTokens
	cachedTokens := st.firstUsage.CachedTokens
	finalNonReasoning := 0
	if st.flushedAny && finalRoundUsage != nil {
		finalNonReasoning = finalRoundUsage.OutputTokens - finalRoundUsage.ReasoningTokens
		if finalNonReasoning < 0 {
			finalNonReasoning = 0
		}
	}
	outputTokens := st.sumReasoning + finalNonReasoning

	usage := map[string]any{
		"input_tokens":  inputTokens,
		"output_tokens": outputTokens,
		"total_tokens":  inputTokens + outputTokens,
		"output_tokens_details": map[string]any{
			"reasoning_tokens": st.sumReasoning,
		},
	}
	if cachedTokens > 0 {
		usage["input_tokens_details"] = map[string]any{"cached_tokens": cachedTokens}
	}
	raw, err := json.Marshal(usage)
	if err != nil {
		return nil
	}
	return raw
}

// syntheticIncompleteEvent synthesizes a response.incomplete terminal event (a fallback for a continue-thinking round
// failure or upstream EOF, so the client does not hang on a stream with no terminal state).
func (st *foldState) syntheticIncompleteEvent(reason string, finalRoundUsage *UsageInfo) []byte {
	base := st.baseResponse
	if len(base) == 0 {
		base = json.RawMessage(`{}`)
	}
	resp := []byte(base)
	outputRaw, err := json.Marshal(st.clientFacingOutput())
	if err != nil {
		outputRaw = []byte(`[]`)
	}
	resp, _ = sjson.SetRawBytes(resp, "output", outputRaw)
	resp, _ = sjson.SetBytes(resp, "status", "incomplete")
	resp, _ = sjson.SetBytes(resp, "incomplete_details.reason", reason)
	if usageRaw := st.rebuildUsage(finalRoundUsage); usageRaw != nil {
		resp, _ = sjson.SetRawBytes(resp, "usage", usageRaw)
	}
	out := []byte(`{}`)
	out, _ = sjson.SetBytes(out, "type", "response.incomplete")
	out, _ = sjson.SetRawBytes(out, "response", resp)
	out, _ = sjson.SetBytes(out, "sequence_number", st.nextSeq())
	return out
}

// roundOutcome is the result of reading one round.
type roundOutcome struct {
	terminal       []byte            // raw bytes of the terminal event (nil = EOF with no terminal)
	usage          *UsageInfo        // this round's usage
	roundReasoning []json.RawMessage // this round's reasoning items (snapshots from output_item.done)
	buffered       []*bufferedOutputItem
	aborted        bool  // forward returned false
	readErr        error // SSE read error
}

// readRound reads one upstream SSE round: reasoning is forwarded in real time, other items are buffered, and the terminal event stops the read.
func (st *foldState) readRound(body io.Reader, roundNo int, f *continueFold) roundOutcome {
	var out roundOutcome
	// upstream output_index → downstream output_index (reasoning type) or buffer slot
	reasoningOI := map[int64]int64{}
	bufferByOI := map[int64]*bufferedOutputItem{}

	out.readErr = ReadSSEStream(body, func(data []byte) bool {
		parsed := gjson.ParseBytes(data)
		eventType := parsed.Get("type").String()

		switch eventType {
		case "response.created", "response.in_progress":
			if roundNo > 1 {
				// lifecycle events of hidden rounds are not sent downstream, but are still fed to TTFT observation.
				f.observe(data)
				return true
			}
			if eventType == "response.created" {
				if resp := parsed.Get("response"); resp.Exists() {
					st.baseResponse = json.RawMessage(resp.Raw)
				}
			}
			if !f.forward(st.rewriteEvent(data, 0, false)) {
				out.aborted = true
				return false
			}
			return true
		case "response.completed", "response.failed", "response.incomplete":
			out.terminal = append([]byte(nil), data...)
			out.usage = extractUsageFromResult(parsed.Get("response.usage"))
			return false
		}

		oi := parsed.Get("output_index")

		if eventType == "response.output_item.added" {
			item := parsed.Get("item")
			if item.Get("type").String() == "reasoning" {
				ds := st.dsOI
				st.dsOI++
				reasoningOI[oi.Int()] = ds
				if !f.forward(st.rewriteEvent(data, ds, true)) {
					out.aborted = true
					return false
				}
				return true
			}
			entry := &bufferedOutputItem{
				upstreamOI: oi.Int(),
				itemType:   item.Get("type").String(),
				events:     [][]byte{append([]byte(nil), data...)},
				item:       json.RawMessage(item.Raw),
			}
			bufferByOI[oi.Int()] = entry
			out.buffered = append(out.buffered, entry)
			f.observe(data)
			return true
		}

		if oi.Exists() {
			if ds, ok := reasoningOI[oi.Int()]; ok {
				if eventType == "response.output_item.done" {
					if item := parsed.Get("item"); item.Exists() {
						ritem := json.RawMessage(item.Raw)
						out.roundReasoning = append(out.roundReasoning, ritem)
						st.finalOutput = append(st.finalOutput, ritem)
					}
				}
				if !f.forward(st.rewriteEvent(data, ds, true)) {
					out.aborted = true
					return false
				}
				return true
			}
			if entry, ok := bufferByOI[oi.Int()]; ok {
				entry.events = append(entry.events, append([]byte(nil), data...))
				if eventType == "response.output_item.done" {
					if item := parsed.Get("item"); item.Exists() {
						entry.item = json.RawMessage(item.Raw)
					}
				}
				f.observe(data)
				return true
			}
		}

		// events that cannot be traced to an owning item (e.g. response.output_text.annotation),
		// are forwarded on a best-effort basis, keeping behavior consistent with the passthrough path.
		if !f.forward(st.rewriteEvent(data, 0, false)) {
			out.aborted = true
			return false
		}
		return true
	})
	return out
}

// flushBuffered flushes one round's buffered tentative output to the downstream as raw bytes (rewriting seq/oi).
func (st *foldState) flushBuffered(buffered []*bufferedOutputItem, f *continueFold) bool {
	for _, entry := range buffered {
		ds := st.dsOI
		st.dsOI++
		for _, ev := range entry.events {
			if !f.forward(st.rewriteEvent(ev, ds, true)) {
				return false
			}
		}
		if len(entry.item) > 0 {
			st.finalOutput = append(st.finalOutput, entry.item)
		}
	}
	st.flushedAny = true
	return true
}

// shouldContinueRound decides after this round whether to continue, and returns (continue?, stop reason).
func shouldContinueRound(terminal []byte, usage *UsageInfo, roundReasoning []json.RawMessage, roundNo, maxRounds int, clientGone bool) (bool, string) {
	if gjson.GetBytes(terminal, "type").String() != "response.completed" {
		return false, continueStopClean
	}
	if usage == nil || !isReasoningTruncationTokens(usage.ReasoningTokens) {
		return false, continueStopClean
	}
	if clientGone {
		return false, continueStopClientGone
	}
	if roundNo >= maxRounds {
		return false, continueStopMaxRounds
	}
	if len(roundReasoning) == 0 {
		return false, continueStopNoEncrypted
	}
	last := roundReasoning[len(roundReasoning)-1]
	if gjson.GetBytes(last, "encrypted_content").String() == "" {
		return false, continueStopNoEncrypted
	}
	return true, ""
}

// runContinueThinkingFold runs the multi-round fold main loop. firstResp is the upstream response for round 1, already confirmed 2xx
// (its resource lifecycle is managed by the caller); later rounds are opened via f.openRound.
func runContinueThinkingFold(firstResp *http.Response, f *continueFold) continueFoldResult {
	st := &foldState{}
	var result continueFoldResult

	// keepalive covers the whole fold, including round 1, which may stay silent for a long time. On stop it synchronously waits for the goroutine to exit,
	// so that after fold returns no keepalive callback can run concurrently with the final flush.
	var keepaliveStop, keepaliveDone chan struct{}
	startKeepalive := func() {
		if f.keepalive == nil || f.keepaliveInterval <= 0 || keepaliveStop != nil {
			return
		}
		keepaliveStop = make(chan struct{})
		keepaliveDone = make(chan struct{})
		go func() {
			defer close(keepaliveDone)
			ticker := time.NewTicker(f.keepaliveInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					if !f.keepalive() {
						return
					}
				case <-keepaliveStop:
					return
				}
			}
		}()
	}
	startKeepalive()
	defer func() {
		if keepaliveStop != nil {
			close(keepaliveStop)
			<-keepaliveDone
		}
	}()

	resp := firstResp
	roundNo := 0
	for {
		roundNo++
		result.RoundsRun = roundNo
		result.FinalResponse = resp
		roundStart := time.Now()
		// record the start of this round's reasoning in finalOutput: if this round becomes the last round,
		// reasoning from earlier rounds before this point has encrypted_content stripped when sent.
		st.lastRoundReasoningStart = len(st.finalOutput)
		outcome := st.readRound(resp.Body, roundNo, f)
		resp.Body.Close()

		statusCode := http.StatusOK
		if outcome.terminal != nil {
			if t := gjson.GetBytes(outcome.terminal, "type").String(); t == "response.failed" || t == "response.incomplete" {
				// a non-completed terminal state must be the final round (fold only continues on completed + fingerprint match),
				// so record the real status rather than always 200, to make statistics distinguishable.
				statusCode = 0
			}
		}
		stat := continueRoundStat{
			Trace:      f.snapshotTrace(),
			Usage:      outcome.usage,
			StatusCode: statusCode,
			DurationMs: int(time.Since(roundStart).Milliseconds()),
		}
		result.Rounds = append(result.Rounds, stat)
		if roundNo == 1 {
			st.firstUsage = outcome.usage
		}
		if outcome.usage != nil {
			st.sumReasoning += outcome.usage.ReasoningTokens
		}
		result.FinalUsage = outcome.usage
		result.ReadErr = outcome.readErr

		if outcome.aborted {
			result.StopReason = continueStopForwardAbort
			return result
		}

		if outcome.terminal == nil {
			// upstream EOF with no terminal. On round 1 with no output at all (including buffered), stay silent so that the upper layer's
			// transparent stream-break retry takes over; otherwise flush the buffered content and synthesize incomplete to give the client a terminal state,
			// so buffered content is not discarded and the client does not get an empty 200 stream.
			result.StopReason = continueStopUpstreamEOF
			if roundNo == 1 && len(st.finalOutput) == 0 && !st.flushedAny && len(outcome.buffered) == 0 {
				return result
			}
			if !st.flushBuffered(outcome.buffered, f) {
				result.StopReason = continueStopForwardAbort
				return result
			}
			f.forward(st.syntheticIncompleteEvent("upstream_eof", outcome.usage))
			result.GotTerminal = true
			return result
		}
		result.GotTerminal = true

		doContinue, stopReason := shouldContinueRound(
			outcome.terminal, outcome.usage, outcome.roundReasoning,
			roundNo, f.maxRounds, f.clientGone(),
		)

		if !doContinue {
			// round 1 failed directly and never wrote fold output to the client: pass the terminal through unchanged,
			// so that the existing failure suppression / account switching / cooldown semantics stay byte-for-byte identical.
			if roundNo == 1 && len(st.finalOutput) == 0 && !st.flushedAny && len(outcome.buffered) == 0 &&
				gjson.GetBytes(outcome.terminal, "type").String() != "response.completed" {
				f.forward(outcome.terminal)
				result.StopReason = stopReason
				return result
			}
			if !st.flushBuffered(outcome.buffered, f) {
				result.StopReason = continueStopForwardAbort
				return result
			}
			f.forward(st.rebuildTerminalEvent(outcome.terminal, outcome.usage))
			result.StopReason = stopReason
			return result
		}

		// continue thinking: this round's reasoning, with id stripped, is appended to the replay tail along with the marker.
		for _, ritem := range outcome.roundReasoning {
			if stripped, ok := stripResponseItemID(ritem); ok {
				st.replayTail = append(st.replayTail, stripped)
			}
		}
		st.replayTail = append(st.replayTail, continueMarkerItem())

		nextBody, err := buildContinuationBody(f.baseBody, st.replayTail)
		if err != nil {
			result.FailedContinuation = &continueRoundStat{ErrMessage: err.Error(), Trace: upstreamTraceSnapshot{RequestID: f.snapshotTrace().RequestID}}
			f.forward(st.syntheticIncompleteEvent("upstream_error", outcome.usage))
			result.StopReason = continueStopRoundError
			return result
		}
		nextResp, err := f.openRound(nextBody)
		if err != nil {
			result.FailedContinuation = &continueRoundStat{ErrMessage: err.Error(), Trace: f.snapshotTrace()}
			f.forward(st.syntheticIncompleteEvent("upstream_error", outcome.usage))
			result.StopReason = continueStopRoundError
			return result
		}
		if nextResp.StatusCode != http.StatusOK {
			errBody, _ := io.ReadAll(io.LimitReader(nextResp.Body, 2048))
			nextResp.Body.Close()
			result.FailedContinuation = &continueRoundStat{
				Trace:      f.snapshotTrace(),
				StatusCode: nextResp.StatusCode,
				ErrMessage: fmt.Sprintf("continuation round rejected: %s", upstreamErrorConsoleBody(errBody)),
			}
			f.forward(st.syntheticIncompleteEvent("upstream_error", outcome.usage))
			result.StopReason = continueStopRoundError
			return result
		}
		resp = nextResp
	}
}
