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
}

// realtimeAudioResponse tracks the assistant response that is currently streaming.
type realtimeAudioResponse struct {
	open       bool
	id         string
	itemID     string
	transcript string
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
}

// newRealtimeAudioMapper creates a mapper; a nil newID uses defaultRealtimeAudioID.
func newRealtimeAudioMapper(newID func(prefix string) string) *realtimeAudioMapper {
	if newID == nil {
		newID = defaultRealtimeAudioID
	}
	return &realtimeAudioMapper{newID: newID}
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
		switch realtimeAudioTurnRole(root) {
		case "assistant":
			m.openResponse(emit)
		case "user":
			m.openUserItem(emit)
			if !m.user.speechStarted {
				m.user.speechStarted = true
				emit(map[string]any{"type": "input_audio_buffer.speech_started", "item_id": m.user.id, "audio_start_ms": 0})
			}
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
		switch realtimeAudioTurnRole(root) {
		case "assistant":
			m.closeResponse(emit, root)
		case "user":
			m.closeUserItem(emit, root)
		}

	case "input_transcript.added":
		m.openUserItem(emit)
		delta := realtimeAudioString(root, "item.text", "delta", "text")
		emit(map[string]any{
			"type":          "conversation.item.input_audio_transcription.delta",
			"item_id":       m.user.id,
			"content_index": 0,
			"delta":         delta,
		})
		m.user.transcript += delta

	case "error":
		m.mapError(emit, root)
	}
	return out
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
	item := map[string]any{
		"id":     itemID,
		"object": "realtime.item",
		"type":   "message",
		"role":   "assistant",
		"status": "completed",
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
	emit(map[string]any{
		"type": "response.done",
		"response": map[string]any{
			"id":     responseID,
			"object": "realtime.response",
			"status": "completed",
			"output": []any{item},
		},
	})

	m.lastItemID = itemID
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
	if transcript == "" {
		transcript = m.user.transcript
	}
	itemID := m.user.id
	if m.user.speechStarted {
		emit(map[string]any{"type": "input_audio_buffer.speech_stopped", "item_id": itemID, "audio_end_ms": 0})
	}
	emit(map[string]any{
		"type":          "conversation.item.input_audio_transcription.completed",
		"item_id":       itemID,
		"content_index": 0,
		"transcript":    transcript,
	})

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
	session := map[string]any{
		"object":            "realtime.session",
		"type":              "realtime",
		"model":             model,
		"instructions":      realtimeAudioInstructionsOrDefault(cfg.Instructions),
		"output_modalities": []string{modality},
		"audio": map[string]any{
			"input": map[string]any{
				"format":         map[string]any{"type": "audio/pcm", "rate": 24000},
				"turn_detection": map[string]any{"type": "server_vad"},
			},
			"output": map[string]any{
				"format": map[string]any{"type": "audio/pcm", "rate": 24000},
				"voice":  voice,
			},
		},
	}
	if id != "" {
		session["id"] = id
	}
	return session
}

// realtimeAudioFormatAccepted reports whether a format field is absent or is 24 kHz mono PCM16.
// It accepts the string "pcm16" or the object {"type":"audio/pcm","rate":24000} (rate optional).
func realtimeAudioFormatAccepted(format gjson.Result) bool {
	if !format.Exists() || format.Type == gjson.Null {
		return true
	}
	if format.Type == gjson.String {
		return strings.EqualFold(format.String(), "pcm16")
	}
	if !format.IsObject() {
		return false
	}
	if !strings.EqualFold(format.Get("type").String(), "audio/pcm") {
		return false
	}
	rate := format.Get("rate")
	return !rate.Exists() || (rate.Type == gjson.Number && rate.Num == 24000)
}

// applyRealtimeAudioSessionUpdate applies an OpenAI session.update event to cfg.
// It returns the sideband update for ChatGPT Live (nil when only the model or modalities changed) and the session.updated ack.
func applyRealtimeAudioSessionUpdate(cfg *realtimeAudioConfig, raw []byte) (sideband, ack []byte, apiErr *api.APIError) {
	session := gjson.GetBytes(raw, "session")
	if !session.IsObject() {
		return nil, nil, api.NewAPIError(api.ErrCodeMissingField, "session is required in session.update", api.ErrorTypeInvalidRequest)
	}

	if !realtimeAudioFormatAccepted(session.Get("input_audio_format")) || !realtimeAudioFormatAccepted(session.Get("audio.input.format")) {
		return nil, nil, api.NewAPIError(api.ErrCodeInvalidRequest, `only pcm16 24 kHz mono input audio is supported (audio.input.format {"type":"audio/pcm","rate":24000})`, api.ErrorTypeInvalidRequest)
	}
	if !realtimeAudioFormatAccepted(session.Get("output_audio_format")) || !realtimeAudioFormatAccepted(session.Get("audio.output.format")) {
		return nil, nil, api.NewAPIError(api.ErrCodeInvalidRequest, `only pcm16 24 kHz mono output audio is supported (audio.output.format {"type":"audio/pcm","rate":24000})`, api.ErrorTypeInvalidRequest)
	}

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

// realtimeTextItemToContextAppend converts an OpenAI conversation.item.create text message into a ChatGPT Live context append.
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
		"type": "session.context.append",
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
