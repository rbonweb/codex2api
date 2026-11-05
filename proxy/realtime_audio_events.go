package proxy

import (
	"encoding/json"
	"strings"

	"github.com/codex2api/api"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

// realtimeAudioConfig is the per-connection audio session state that OpenAI session.update events change.
type realtimeAudioConfig struct {
	Model        string
	Voice        string
	Instructions string
	TextOnly     bool
	// InputRate and OutputRate are the PCM16 sample rates the client uses; zero means the native 24000.
	InputRate  int
	OutputRate int
	// DelegationNoAck asks Live not to speak a filler acknowledgement while a client delegation runs (delegation.acknowledgement "none").
	DelegationNoAck bool
	// TranscriptionLanguage is audio.input.transcription.language (ISO 639-1, lower case): the language the user speaks. It is reported on every input transcript, adds one developer message at call creation and, for Gujarati, converts Devanagari transcripts to Gujarati script.
	TranscriptionLanguage string
	// NoCreateResponse records turn_detection.create_response:false. Live still decides when to speak, so it only changes what the session object reports; the bridge never sends response.create on its own, and a client response.create is forwarded either way.
	NoCreateResponse bool
}

const (
	realtimeAudioNativeRate = 24000
	realtimeAudioLowRate    = 16000
)

// inputRate returns the effective client input sample rate.
func (cfg *realtimeAudioConfig) inputRate() int {
	if cfg.InputRate == realtimeAudioLowRate {
		return realtimeAudioLowRate
	}
	return realtimeAudioNativeRate
}

// outputRate returns the effective client output sample rate.
func (cfg *realtimeAudioConfig) outputRate() int {
	if cfg.OutputRate == realtimeAudioLowRate {
		return realtimeAudioLowRate
	}
	return realtimeAudioNativeRate
}

// realtimeAudioParamError marks an API error that names the session field behind it; the bridge writes it as the public error event with param.
type realtimeAudioParamError struct {
	Param string `json:"param"`
}

// realtimeAudioFieldError builds a session.update error with a code (unknown_parameter, unsupported_value, ...) and the dotted param.
func realtimeAudioFieldError(code, param, message string) *api.APIError {
	return api.NewAPIErrorWithDetails(api.ErrorCode(code), message, api.ErrorTypeInvalidRequest, realtimeAudioParamError{Param: param})
}

// Session fields OpenAI Realtime clients commonly send. Fields the bridge cannot act on (tools, temperature and so on) are tolerated and ignored because ChatGPT Live takes only instructions, voice and delegation at call creation; any other key is unknown_parameter.
var (
	realtimeAudioSessionKeys = setOf("type", "id", "object", "expires_at", "client_secret", "model", "instructions", "voice", "audio", "modalities", "output_modalities", "input_audio_format", "output_audio_format", "input_audio_transcription", "input_audio_noise_reduction", "turn_detection", "tools", "tool_choice", "temperature", "max_response_output_tokens", "max_output_tokens", "speed", "include", "tracing", "prompt", "truncation", "delegation")
	realtimeAudioAudioKeys   = setOf("input", "output")
	realtimeAudioInputKeys   = setOf("format", "transcription", "noise_reduction", "turn_detection")
	realtimeAudioOutputKeys  = setOf("format", "voice", "speed")
)

func setOf(keys ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		set[key] = struct{}{}
	}
	return set
}

// firstUnknownKey returns the dotted path of the first key of obj (sorted by position) that is not in known, or "".
func firstUnknownKey(obj gjson.Result, prefix string, known map[string]struct{}) string {
	unknown := ""
	obj.ForEach(func(key, _ gjson.Result) bool {
		if _, ok := known[key.String()]; !ok {
			unknown = prefix + "." + key.String()
			return false
		}
		return true
	})
	return unknown
}

// validateRealtimeAudioSessionKeys rejects session keys the bridge does not know.
func validateRealtimeAudioSessionKeys(session gjson.Result) *api.APIError {
	check := func(obj gjson.Result, prefix string, known map[string]struct{}) *api.APIError {
		if !obj.IsObject() {
			return nil
		}
		if param := firstUnknownKey(obj, prefix, known); param != "" {
			return realtimeAudioFieldError("unknown_parameter", param, "Unknown parameter: '"+param+"'.")
		}
		return nil
	}
	for _, step := range []struct {
		obj    gjson.Result
		prefix string
		known  map[string]struct{}
	}{
		{session, "session", realtimeAudioSessionKeys},
		{session.Get("audio"), "session.audio", realtimeAudioAudioKeys},
		{session.Get("audio.input"), "session.audio.input", realtimeAudioInputKeys},
		{session.Get("audio.output"), "session.audio.output", realtimeAudioOutputKeys},
	} {
		if err := check(step.obj, step.prefix, step.known); err != nil {
			return err
		}
	}
	return nil
}

// realtimeAudioResponse tracks the assistant response that is currently streaming.
type realtimeAudioResponse struct {
	open       bool
	id         string
	itemID     string
	transcript string
	// interrupted is set when the user spoke over the response: it then ends as cancelled with reason turn_detected.
	interrupted bool
}

// realtimeAudioUserItem tracks the user item that is currently being transcribed.
type realtimeAudioUserItem struct {
	open          bool
	id            string
	transcript    string
	speechStarted bool
}

// realtimeAudioMapper turns ChatGPT Live sideband events into OpenAI Realtime server events.
// It holds the open response and user item, so it must be used by one connection only.
type realtimeAudioMapper struct {
	newID func(prefix string) string

	assistant     realtimeAudioResponse
	user          realtimeAudioUserItem
	pendingUserID string
	lastItemID    string

	// lastResponseID and lastResponseItemID name the response that ended last, for an interruption reported after its turn.done.
	lastResponseID     string
	lastResponseItemID string
	barge              *audioBargeIn
	speech             audioSpeechTracker
	// language is the transcription language the client named ("" detects it); fixScript converts input transcripts to its script.
	language  string
	fixScript func(string) string
}

// newRealtimeAudioMapper creates a mapper; a nil newID uses defaultRealtimeAudioID.
func newRealtimeAudioMapper(newID func(prefix string) string) *realtimeAudioMapper {
	if newID == nil {
		newID = defaultRealtimeAudioID
	}
	return &realtimeAudioMapper{newID: newID, barge: newAudioBargeIn(nil)}
}

// SetTranscriptionLanguage fixes the language reported on input transcripts and the script they are converted to.
func (m *realtimeAudioMapper) SetTranscriptionLanguage(code string) {
	m.language, m.fixScript = code, transcriptionScriptFix(code)
}

// Speech turns the detector's changes on the client's audio into input_audio_buffer.speech_started and input_audio_buffer.speech_stopped with the item the speech will belong to. They come well before the ones derived from the upstream turn events, which then stay silent for a turn the detector saw.
func (m *realtimeAudioMapper) Speech(events []audioSpeechEvent) [][]byte {
	var out [][]byte
	for _, ev := range events {
		m.speech.Detector(ev)
		payload := map[string]any{"type": "input_audio_buffer.speech_stopped", "audio_end_ms": ev.MS, "item_id": m.UserItemID(), "event_id": m.newID("event")}
		if ev.Started {
			payload["type"] = "input_audio_buffer.speech_started"
			delete(payload, "audio_end_ms")
			payload["audio_start_ms"] = ev.MS
		}
		out = append(out, marshalRealtimeServerEvent(payload))
	}
	return out
}

// interrupt reports that the user spoke over the assistant: the open response ends as cancelled, and the extension event response.output_audio.interrupted names the response (the last one when it already ended).
func (m *realtimeAudioMapper) interrupt(emit func(map[string]any)) {
	responseID, itemID := m.lastResponseID, m.lastResponseItemID
	if m.assistant.open {
		m.assistant.interrupted = true
		responseID, itemID = m.assistant.id, m.assistant.itemID
	}
	emit(map[string]any{"type": "response.output_audio.interrupted", "response_id": responseID, "item_id": itemID})
}

// defaultRealtimeAudioID returns prefix plus a random UUID without dashes, cut to 24 characters.
func defaultRealtimeAudioID(prefix string) string {
	id := prefix + "_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if len(id) > 24 {
		id = id[:24]
	}
	return id
}

// UserItemID returns the id of the open user item, or the id the next user item will use.
// Calling it again before a user turn starts returns the same id.
func (m *realtimeAudioMapper) UserItemID() string {
	if m.user.open {
		return m.user.id
	}
	if m.pendingUserID == "" {
		m.pendingUserID = m.newID("item")
	}
	return m.pendingUserID
}

// LastItemID returns the id of the last completed item in the conversation, or an empty string before the first one.
func (m *realtimeAudioMapper) LastItemID() string {
	return m.lastItemID
}

// MapUpstream converts one ChatGPT Live sideband event into zero or more OpenAI Realtime events.
// Invalid JSON and events without a mapping yield nil.
func (m *realtimeAudioMapper) MapUpstream(raw []byte) [][]byte {
	if !gjson.ValidBytes(raw) {
		return nil
	}
	root := gjson.ParseBytes(raw)
	if !root.IsObject() {
		return nil
	}

	var out [][]byte
	emit := func(payload map[string]any) {
		payload["event_id"] = m.newID("event")
		out = append(out, marshalRealtimeServerEvent(payload))
	}

	switch root.Get("type").String() {
	case "turn.created":
		role := realtimeAudioTurnRole(root)
		switch role {
		case "assistant":
			m.openResponse(emit)
		case "user":
			m.openUserItem(emit)
			// A turn the detector did not hear is reported from the upstream event, on the session clock.
			if m.speech.TurnCreated() {
				m.user.speechStarted = true
				emit(map[string]any{"type": "input_audio_buffer.speech_started", "item_id": m.user.id, "audio_start_ms": root.Get("turn.start_ms").Int()})
			}
		}
		if _, interrupted := m.barge.Turn("created", role, root.Get("turn.id").String()); interrupted {
			m.interrupt(emit)
		}

	case "session.output_audio.delta", "output_audio.delta":
		m.openResponse(emit)
		emit(map[string]any{
			"type":          "response.output_audio.delta",
			"response_id":   m.assistant.id,
			"item_id":       m.assistant.itemID,
			"output_index":  0,
			"content_index": 0,
			"delta":         realtimeAudioString(root, "delta", "audio"),
		})

	case "output_transcript.added":
		m.openResponse(emit)
		delta := realtimeAudioString(root, "item.text", "delta", "text")
		emit(map[string]any{
			"type":          "response.output_audio_transcript.delta",
			"response_id":   m.assistant.id,
			"item_id":       m.assistant.itemID,
			"output_index":  0,
			"content_index": 0,
			"delta":         delta,
		})
		m.assistant.transcript += delta

	case "turn.done":
		role := realtimeAudioTurnRole(root)
		switch role {
		case "assistant":
			m.closeResponse(emit, root)
		case "user":
			m.closeUserItem(emit, root)
		}
		if _, interrupted := m.barge.Turn("done", role, root.Get("turn.id").String()); interrupted {
			m.interrupt(emit)
		}

	case "input_transcript.added":
		m.openUserItem(emit)
		delta := realtimeAudioString(root, "item.text", "delta", "text")
		if m.fixScript != nil {
			delta = m.fixScript(delta)
		}
		m.user.transcript += delta
		event := map[string]any{
			"type":          "conversation.item.input_audio_transcription.delta",
			"item_id":       m.user.id,
			"content_index": 0,
			"delta":         delta,
		}
		// Gateway extension: language is the one the client named, or the detector's guess on the transcript so far.
		if lang := m.transcriptLanguage(m.user.transcript); lang != "" {
			event["language"] = lang
			if script := m.transcriptScript(lang, m.user.transcript); script != "" {
				event["script"] = script
			}
		}
		emit(event)

	case "error":
		m.mapError(emit, root)
	}
	return out
}

// transcriptLanguage returns the language to report for an input transcript: the configured one, else the detector's answer on the last 200 bytes (empty when it is not reliable).
func (m *realtimeAudioMapper) transcriptLanguage(transcript string) string {
	if m.language != "" {
		return m.language
	}
	return liveTranscriptLanguage(liveTrimTail(transcript, liveLanguageWindow))
}

// transcriptScript reports script "arab" when the client pinned a language that is not written in Arabic script and the transcript came back in it (Gujarati speech recognised as Urdu); empty otherwise and for detected languages.
func (m *realtimeAudioMapper) transcriptScript(lang, transcript string) string {
	if m.language == "" {
		return ""
	}
	return transcriptScript(lang, liveTrimTail(transcript, liveLanguageWindow))
}

// realtimeAudioTurnRole reads the speaker of a turn from turn.role, falling back to the top-level role.
func realtimeAudioTurnRole(root gjson.Result) string {
	role := root.Get("turn.role").String()
	if role == "" {
		role = root.Get("role").String()
	}
	return strings.ToLower(role)
}

// realtimeAudioString returns the first non-empty string found at the given paths.
func realtimeAudioString(root gjson.Result, paths ...string) string {
	for _, path := range paths {
		if value := root.Get(path); value.Type == gjson.String && value.String() != "" {
			return value.String()
		}
	}
	return ""
}

// openResponse starts an assistant response unless one is already open.
func (m *realtimeAudioMapper) openResponse(emit func(map[string]any)) {
	if m.assistant.open {
		return
	}
	m.assistant = realtimeAudioResponse{
		open:   true,
		id:     m.newID("resp"),
		itemID: m.newID("item"),
	}
	emit(map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id":     m.assistant.id,
			"object": "realtime.response",
			"status": "in_progress",
			"output": []any{},
		},
	})
	emit(map[string]any{
		"type":         "response.output_item.added",
		"response_id":  m.assistant.id,
		"output_index": 0,
		"item": map[string]any{
			"id":      m.assistant.itemID,
			"object":  "realtime.item",
			"type":    "message",
			"role":    "assistant",
			"status":  "in_progress",
			"content": []any{},
		},
	})
	emit(map[string]any{
		"type":          "response.content_part.added",
		"response_id":   m.assistant.id,
		"item_id":       m.assistant.itemID,
		"output_index":  0,
		"content_index": 0,
		"part":          map[string]any{"type": "audio", "transcript": ""},
	})
}

// closeResponse finishes the assistant response, opening one first when the turn was silent.
func (m *realtimeAudioMapper) closeResponse(emit func(map[string]any), turn gjson.Result) {
	m.openResponse(emit)
	transcript := turn.Get("turn.transcript").String()
	if transcript == "" {
		transcript = m.assistant.transcript
	}
	responseID := m.assistant.id
	itemID := m.assistant.itemID
	status, itemStatus := "completed", "completed"
	if m.assistant.interrupted {
		status, itemStatus = "cancelled", "incomplete"
	}
	item := map[string]any{
		"id":     itemID,
		"object": "realtime.item",
		"type":   "message",
		"role":   "assistant",
		"status": itemStatus,
		"content": []any{
			map[string]any{"type": "output_audio", "transcript": transcript},
		},
	}

	emit(map[string]any{
		"type":          "response.output_audio.done",
		"response_id":   responseID,
		"item_id":       itemID,
		"output_index":  0,
		"content_index": 0,
	})
	emit(map[string]any{
		"type":          "response.output_audio_transcript.done",
		"response_id":   responseID,
		"item_id":       itemID,
		"output_index":  0,
		"content_index": 0,
		"transcript":    transcript,
	})
	emit(map[string]any{
		"type":          "response.content_part.done",
		"response_id":   responseID,
		"item_id":       itemID,
		"output_index":  0,
		"content_index": 0,
		"part":          map[string]any{"type": "audio", "transcript": transcript},
	})
	emit(map[string]any{
		"type":         "response.output_item.done",
		"response_id":  responseID,
		"output_index": 0,
		"item":         item,
	})
	response := map[string]any{
		"id":     responseID,
		"object": "realtime.response",
		"status": status,
		"output": []any{item},
	}
	if status == "cancelled" {
		response["status_details"] = map[string]any{"type": "cancelled", "reason": "turn_detected"}
	}
	emit(map[string]any{"type": "response.done", "response": response})

	m.lastItemID = itemID
	m.lastResponseID, m.lastResponseItemID = responseID, itemID
	m.assistant = realtimeAudioResponse{}
}

// openUserItem creates the user item in the conversation unless it is already open.
func (m *realtimeAudioMapper) openUserItem(emit func(map[string]any)) {
	if m.user.open {
		return
	}
	id := m.UserItemID()
	m.pendingUserID = ""
	m.user = realtimeAudioUserItem{open: true, id: id}

	var previous any
	if m.lastItemID != "" {
		previous = m.lastItemID
	}
	emit(map[string]any{
		"type":             "conversation.item.created",
		"previous_item_id": previous,
		"item": map[string]any{
			"id":      id,
			"object":  "realtime.item",
			"type":    "message",
			"role":    "user",
			"status":  "completed",
			"content": []any{map[string]any{"type": "input_audio", "transcript": nil}},
		},
	})
}

// closeUserItem completes the user transcription, opening the item first when no delta arrived.
func (m *realtimeAudioMapper) closeUserItem(emit func(map[string]any), turn gjson.Result) {
	m.openUserItem(emit)
	transcript := turn.Get("turn.transcript").String()
	if m.fixScript != nil {
		transcript = m.fixScript(transcript)
	}
	if transcript == "" {
		transcript = m.user.transcript
	}
	itemID := m.user.id
	if m.speech.TurnDone() {
		emit(map[string]any{"type": "input_audio_buffer.speech_stopped", "item_id": itemID, "audio_end_ms": turn.Get("turn.end_ms").Int()})
	}
	completed := map[string]any{
		"type":          "conversation.item.input_audio_transcription.completed",
		"item_id":       itemID,
		"content_index": 0,
		"transcript":    transcript,
	}
	if lang := m.transcriptLanguage(transcript); lang != "" {
		completed["language"] = lang
		if script := m.transcriptScript(lang, transcript); script != "" {
			completed["script"] = script
		}
	}
	emit(completed)

	m.lastItemID = itemID
	m.user = realtimeAudioUserItem{}
}

// mapError converts an upstream error into an OpenAI error event with defaults for missing fields.
func (m *realtimeAudioMapper) mapError(emit func(map[string]any), root gjson.Result) {
	errObj := root.Get("error")
	errType := errObj.Get("type").String()
	if errType == "" {
		errType = "server_error"
	}
	code := errObj.Get("code").String()
	if code == "" {
		code = root.Get("code").String()
	}
	message := errObj.Get("message").String()
	if errObj.Type == gjson.String {
		message = errObj.String()
	}
	if message == "" {
		message = root.Get("message").String()
	}
	if message == "" {
		message = "ChatGPT Live error"
	}

	var codeValue any
	if code != "" {
		codeValue = code
	}
	emit(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    errType,
			"code":    codeValue,
			"message": message,
		},
	})
}

// realtimeAudioSessionObject returns the OpenAI realtime session object for cfg; an empty id leaves out the id field.
func realtimeAudioSessionObject(cfg *realtimeAudioConfig, id string) map[string]any {
	model := cfg.Model
	if strings.TrimSpace(model) == "" {
		model = liveAudioDefaultModel
	}
	voice := cfg.Voice
	if strings.TrimSpace(voice) == "" {
		voice = liveAudioDefaultVoice
	}
	modality := "audio"
	if cfg.TextOnly {
		modality = "text"
	}
	delegation := map[string]any{"type": "client"}
	if cfg.DelegationNoAck {
		delegation["acknowledgement"] = "none"
	}
	session := map[string]any{
		"object":            "realtime.session",
		"type":              "realtime",
		"model":             model,
		"instructions":      realtimeAudioInstructionsOrDefault(cfg.Instructions),
		"output_modalities": []string{modality},
		"audio": map[string]any{
			"input": map[string]any{
				"format":         map[string]any{"type": "audio/pcm", "rate": cfg.inputRate()},
				"turn_detection": map[string]any{"type": "server_vad", "create_response": !cfg.NoCreateResponse},
			},
			"output": map[string]any{
				"format": map[string]any{"type": "audio/pcm", "rate": cfg.outputRate()},
				"voice":  voice,
			},
		},
		"delegation": delegation,
	}
	if cfg.TranscriptionLanguage != "" {
		session["audio"].(map[string]any)["input"].(map[string]any)["transcription"] = map[string]any{"language": cfg.TranscriptionLanguage}
	}
	if id != "" {
		session["id"] = id
	}
	return session
}

// realtimeAudioFormatRate reads a format field. An absent or null field is ok with present false. The string "pcm16" and the object {"type":"audio/pcm","rate":24000|16000} (rate optional, default 24000) are ok with the rate; anything else is not ok. OpenAI has no legacy string for 16 kHz, so that rate exists only in the object form.
func realtimeAudioFormatRate(format gjson.Result) (rate int, present, ok bool) {
	if !format.Exists() || format.Type == gjson.Null {
		return 0, false, true
	}
	if format.Type == gjson.String {
		return realtimeAudioNativeRate, true, strings.EqualFold(format.String(), "pcm16")
	}
	if !format.IsObject() || !strings.EqualFold(format.Get("type").String(), "audio/pcm") {
		return 0, true, false
	}
	value := format.Get("rate")
	if !value.Exists() {
		return realtimeAudioNativeRate, true, true
	}
	if value.Type == gjson.Number && (value.Num == realtimeAudioNativeRate || value.Num == realtimeAudioLowRate) {
		return int(value.Num), true, true
	}
	return 0, true, false
}

// realtimeAudioApplyFormats reads the input and output format fields (the object form wins over the legacy string) into next.
func realtimeAudioApplyFormats(session gjson.Result, next *realtimeAudioConfig) *api.APIError {
	for _, field := range []struct {
		legacy, object, name string
		target               *int
	}{
		{"input_audio_format", "audio.input.format", "input", &next.InputRate},
		{"output_audio_format", "audio.output.format", "output", &next.OutputRate},
	} {
		for _, path := range []string{field.legacy, field.object} {
			rate, present, ok := realtimeAudioFormatRate(session.Get(path))
			if !ok {
				return realtimeAudioFieldError("unsupported_value", "session."+path, `only pcm16 mono `+field.name+` audio at 24 kHz or 16 kHz is supported (`+path+` {"type":"audio/pcm","rate":24000})`)
			}
			if present {
				*field.target = rate
			}
		}
	}
	return nil
}

// realtimeAudioApplyDelegation reads session.delegation. Only client delegation exists; acknowledgement "none" is the gateway extension that turns Live's filler acknowledgement off, "auto" turns it back on. A delegation without acknowledgement leaves the current setting, so it keeps what the delegation query parameter chose.
func realtimeAudioApplyDelegation(session gjson.Result, next *realtimeAudioConfig) *api.APIError {
	delegation := session.Get("delegation")
	if !delegation.Exists() || delegation.Type == gjson.Null {
		return nil
	}
	if !delegation.IsObject() {
		return realtimeAudioFieldError("unsupported_value", "session.delegation", `delegation must be an object such as {"type":"client"}.`)
	}
	if kind := delegation.Get("type"); kind.Exists() && kind.String() != "client" {
		return realtimeAudioFieldError("unsupported_value", "session.delegation.type", "Only client delegation is supported; handle the task in your application and return the result with session.commentary.append.")
	}
	if ack := delegation.Get("acknowledgement"); ack.Exists() && ack.Type != gjson.Null {
		switch ack.String() {
		case "none":
			next.DelegationNoAck = true
		case "auto", "default":
			// "auto" is the documented value; "default" is an undocumented synonym.
			next.DelegationNoAck = false
		default:
			return realtimeAudioFieldError("unsupported_value", "session.delegation.acknowledgement", `The supported acknowledgements are "auto" and "none".`)
		}
	}
	return nil
}

// realtimeAudioApplyTranscription reads the transcription language: audio.input.transcription.language (OpenAI Realtime GA) or the legacy input_audio_transcription.language, an ISO 639-1 code. null clears it. The model and prompt of the transcription block are tolerated and ignored.
func realtimeAudioApplyTranscription(session gjson.Result, next *realtimeAudioConfig) *api.APIError {
	for _, path := range []string{"input_audio_transcription.language", "audio.input.transcription.language"} {
		value := session.Get(path)
		if !value.Exists() {
			continue
		}
		if value.Type == gjson.Null {
			next.TranscriptionLanguage = ""
			continue
		}
		code, ok := normalizeTranscriptionLanguage(value.String())
		if value.Type != gjson.String || !ok {
			return realtimeAudioFieldError("invalid_value", "session."+path, "The transcription language must be an ISO 639-1 code such as en, hi or gu.")
		}
		next.TranscriptionLanguage = code
	}
	return nil
}

// realtimeAudioApplyTurnDetection records turn_detection.create_response. Live runs its own voice activity detection, so the rest of turn_detection is tolerated and ignored.
func realtimeAudioApplyTurnDetection(session gjson.Result, next *realtimeAudioConfig) *api.APIError {
	for _, path := range []string{"turn_detection", "audio.input.turn_detection"} {
		detection := session.Get(path)
		if !detection.IsObject() {
			continue
		}
		if value := detection.Get("create_response"); value.Exists() && value.Type != gjson.Null {
			if value.Type != gjson.True && value.Type != gjson.False {
				return realtimeAudioFieldError("unsupported_value", "session."+path+".create_response", "create_response must be a boolean.")
			}
			next.NoCreateResponse = value.Type == gjson.False
		}
	}
	return nil
}

// applyRealtimeAudioSessionUpdate applies an OpenAI session.update event to cfg.
// It returns the sideband update for ChatGPT Live (nil when only the model or modalities changed) and the session.updated ack.
func applyRealtimeAudioSessionUpdate(cfg *realtimeAudioConfig, raw []byte) (sideband, ack []byte, apiErr *api.APIError) {
	session := gjson.GetBytes(raw, "session")
	if !session.IsObject() {
		return nil, nil, api.NewAPIError(api.ErrCodeMissingField, "session is required in session.update", api.ErrorTypeInvalidRequest)
	}

	if apiErr = validateRealtimeAudioSessionKeys(session); apiErr != nil {
		return nil, nil, apiErr
	}
	// Validate everything on a copy so a rejected update changes nothing.
	next := *cfg
	for _, apply := range []func(gjson.Result, *realtimeAudioConfig) *api.APIError{realtimeAudioApplyFormats, realtimeAudioApplyDelegation, realtimeAudioApplyTranscription, realtimeAudioApplyTurnDetection} {
		if apiErr = apply(session, &next); apiErr != nil {
			return nil, nil, apiErr
		}
	}
	cfg.InputRate, cfg.OutputRate, cfg.DelegationNoAck, cfg.NoCreateResponse = next.InputRate, next.OutputRate, next.DelegationNoAck, next.NoCreateResponse
	cfg.TranscriptionLanguage = next.TranscriptionLanguage

	voiceChanged := false
	if voice := session.Get("audio.output.voice"); voice.Exists() {
		cfg.Voice = voice.String()
		voiceChanged = true
	} else if voice := session.Get("voice"); voice.Exists() {
		cfg.Voice = voice.String()
		voiceChanged = true
	}

	instructionsChanged := false
	if instructions := session.Get("instructions"); instructions.Exists() {
		cfg.Instructions = instructions.String()
		instructionsChanged = true
	}

	if model := session.Get("model"); model.Exists() {
		cfg.Model = model.String()
	}

	modalities := session.Get("output_modalities")
	if !modalities.Exists() {
		modalities = session.Get("modalities")
	}
	if modalities.Exists() {
		cfg.TextOnly = !realtimeModalitiesContainAudio(modalities)
	}

	if instructionsChanged || voiceChanged {
		sideband = marshalRealtimeServerEvent(map[string]any{
			"type": "session.update",
			"session": map[string]any{
				"instructions": realtimeAudioInstructionsOrDefault(cfg.Instructions),
				"audio": map[string]any{
					"output": map[string]any{"voice": mapRealtimeVoiceToLive(cfg.Voice)},
				},
				"delegation": map[string]any{"type": "client"},
			},
		})
	}

	ack = marshalRealtimeServerEvent(map[string]any{
		"type":     "session.updated",
		"event_id": defaultRealtimeAudioID("event"),
		"session":  realtimeAudioSessionObject(cfg, ""),
	})
	return sideband, ack, nil
}

// realtimeAudioInstructionsOrDefault returns the instructions, or the default when they are blank.
func realtimeAudioInstructionsOrDefault(instructions string) string {
	if strings.TrimSpace(instructions) == "" {
		return liveAudioDefaultInstructions
	}
	return instructions
}

// realtimeTextItemToContextAppend converts an OpenAI conversation.item.create text message into a ChatGPT Live context append on the quiet "commentary" channel: the model may use it but does not speak it, which is what a client adding conversation context expects. The speakable default would make Live read the text aloud.
// It returns the sideband event and the conversation.item.created ack; audio items are rejected.
func realtimeTextItemToContextAppend(raw []byte) (sideband, ack []byte, apiErr *api.APIError) {
	item := gjson.GetBytes(raw, "item")
	if !item.IsObject() {
		return nil, nil, api.NewAPIError(api.ErrCodeMissingField, "item is required in conversation.item.create", api.ErrorTypeInvalidRequest)
	}
	if itemType := item.Get("type").String(); itemType != "" && itemType != "message" {
		return nil, nil, api.NewAPIError(api.ErrCodeInvalidRequest, "only message conversation items are supported", api.ErrorTypeInvalidRequest)
	}

	content := item.Get("content")
	if !content.IsArray() {
		return nil, nil, api.NewAPIError(api.ErrCodeInvalidRequest, "conversation item content must be an array of text parts", api.ErrorTypeInvalidRequest)
	}
	var texts []string
	for _, part := range content.Array() {
		partType := part.Get("type").String()
		switch {
		case strings.Contains(strings.ToLower(partType), "audio"):
			return nil, nil, api.NewAPIError(api.ErrCodeInvalidRequest, "audio conversation items are not supported; stream audio with input_audio_buffer.append", api.ErrorTypeInvalidRequest)
		case partType == "input_text" || partType == "text" || partType == "output_text":
			if text := part.Get("text").String(); text != "" {
				texts = append(texts, text)
			}
		default:
			return nil, nil, api.NewAPIError(api.ErrCodeInvalidRequest, "unsupported conversation item content part type: "+partType, api.ErrorTypeInvalidRequest)
		}
	}
	joined := strings.Join(texts, "\n")
	if joined == "" {
		return nil, nil, api.NewAPIError(api.ErrCodeInvalidRequest, "conversation item has no text content", api.ErrorTypeInvalidRequest)
	}

	sideband = marshalRealtimeServerEvent(map[string]any{
		"type":    "session.context.append",
		"channel": "commentary",
		"content": []map[string]any{
			{"type": "input_text", "text": joined},
		},
	})

	var itemMap map[string]any
	if err := json.Unmarshal([]byte(item.Raw), &itemMap); err != nil || itemMap == nil {
		return nil, nil, api.NewAPIError(api.ErrCodeInvalidRequest, "invalid conversation item", api.ErrorTypeInvalidRequest)
	}
	if id, _ := itemMap["id"].(string); strings.TrimSpace(id) == "" {
		itemMap["id"] = defaultRealtimeAudioID("item")
	}
	itemMap["status"] = "completed"

	var previous any
	if prev := gjson.GetBytes(raw, "previous_item_id"); prev.Type == gjson.String && prev.String() != "" {
		previous = prev.String()
	}
	ack = marshalRealtimeServerEvent(map[string]any{
		"type":             "conversation.item.created",
		"event_id":         defaultRealtimeAudioID("event"),
		"previous_item_id": previous,
		"item":             itemMap,
	})
	return sideband, ack, nil
}
