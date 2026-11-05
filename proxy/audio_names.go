package proxy

import (
	"encoding/json"
	"strings"
)

// liveAudioDefaultModel is the ChatGPT Live model used for every OpenAI Realtime audio model.
const liveAudioDefaultModel = "gpt-live-1-codex"

// liveAudioDefaultVoice is the ChatGPT Live voice used when the client names none or an unknown one.
const liveAudioDefaultVoice = "cove"

// liveAudioDefaultInstructions is the system instruction used when the client sends none.
const liveAudioDefaultInstructions = "You are a helpful, concise voice assistant."

// realtimeVoiceAlias maps one OpenAI Realtime voice name to the ChatGPT Live voice that stands in for it.
type realtimeVoiceAlias struct {
	OpenAI string
	Live   string
}

// realtimeVoiceAliases lists the OpenAI voices in a fixed order, for docs and tests.
var realtimeVoiceAliases = []realtimeVoiceAlias{
	{OpenAI: "alloy", Live: "cove"},
	{OpenAI: "ash", Live: "arbor"},
	{OpenAI: "ballad", Live: "vale"},
	{OpenAI: "coral", Live: "ember"},
	{OpenAI: "echo", Live: "spruce"},
	{OpenAI: "sage", Live: "sol"},
	{OpenAI: "shimmer", Live: "juniper"},
	{OpenAI: "verse", Live: "breeze"},
	{OpenAI: "marin", Live: "maple"},
	{OpenAI: "cedar", Live: "arbor"},
}

// liveVoiceNames are the voices ChatGPT Live accepts directly; they pass through unchanged.
var liveVoiceNames = map[string]bool{
	"arbor":   true,
	"breeze":  true,
	"cove":    true,
	"ember":   true,
	"juniper": true,
	"maple":   true,
	"sol":     true,
	"spruce":  true,
	"vale":    true,
}

// isRealtimeAudioModel reports whether a model name selects the audio bridge instead of the text Realtime path.
func isRealtimeAudioModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	switch {
	case model == "":
		return false
	case model == "gpt-realtime" || strings.HasPrefix(model, "gpt-realtime-"):
		return true
	case strings.HasPrefix(model, "gpt-4o-realtime-preview"):
		return true
	case strings.HasPrefix(model, "gpt-4o-mini-realtime-preview"):
		return true
	case model == "gpt-live" || strings.HasPrefix(model, "gpt-live-"):
		return true
	default:
		return false
	}
}

// mapRealtimeModelToLive returns the ChatGPT Live model for a Realtime model name.
// Every audio model maps to the default Live model; the explicit Live name is kept as is.
func mapRealtimeModelToLive(model string) string {
	return liveAudioDefaultModel
}

// mapRealtimeVoiceToLive returns the ChatGPT Live voice for a Realtime voice name.
func mapRealtimeVoiceToLive(voice string) string {
	voice = strings.ToLower(strings.TrimSpace(voice))
	if liveVoiceNames[voice] {
		return voice
	}
	for _, alias := range realtimeVoiceAliases {
		if alias.OpenAI == voice {
			return alias.Live
		}
	}
	return liveAudioDefaultVoice
}

// liveInstructionsItem is the developer message that carries the instructions text as an initial item.
// ChatGPT Live ignores the call-create instructions field but honours the same text as a developer message (proven live on 2026-11-05), so every call sends the text both ways.
func liveInstructionsItem(instructions string) map[string]any {
	return map[string]any{
		"type": "message",
		"role": "developer",
		"content": []map[string]any{
			{"type": "input_text", "text": instructions},
		},
	}
}

// buildLiveAudioSession returns the session.update body sent to ChatGPT Live when a conversational call is created (/v1/realtime and /v1/realtime/calls).
// The effective instructions (the default when the client gave none) go in the instructions field and as the only initial_items developer message.
func buildLiveAudioSession(model, voice, instructions string) json.RawMessage {
	return buildLiveAudioSessionWith(model, voice, instructions, true)
}

// buildLiveTranscriptionSession is the call-create session for /v1/audio/transcriptions: the instructions field only, with no developer item.
// A developer item carrying the stay-silent text stops ChatGPT Live from completing the user turn, so the transcript times out (live QA, 2026-11-05).
func buildLiveTranscriptionSession(instructions string) json.RawMessage {
	return buildLiveAudioSessionWith(liveAudioDefaultModel, "", instructions, false)
}

func buildLiveAudioSessionWith(model, voice, instructions string, instructionsItem bool) json.RawMessage {
	if strings.TrimSpace(instructions) == "" {
		instructions = liveAudioDefaultInstructions
	}
	session := map[string]any{
		"model":        mapRealtimeModelToLive(model),
		"instructions": instructions,
		"audio": map[string]any{
			"output": map[string]any{"voice": mapRealtimeVoiceToLive(voice)},
		},
		"delegation": map[string]any{"type": "client"},
	}
	if instructionsItem {
		session["initial_items"] = []any{liveInstructionsItem(instructions)}
	}
	raw, _ := json.Marshal(session)
	return raw
}

// liveSessionDefaultModel is the public GPT-Live model name used when a session names none.
const liveSessionDefaultModel = "gpt-live-1"

// liveSessionDefaultVoice is the public GPT-Live voice used when a session names none.
const liveSessionDefaultVoice = "marin"

// liveSessionVoiceAlias maps one public GPT-Live voice name to the ChatGPT Live voice that stands in for it.
type liveSessionVoiceAlias struct {
	Public string
	Live   string
}

// liveSessionVoiceAliases lists the public GPT-Live voices in the order of the public voice table.
// ChatGPT Live only has nine voices, so the 22 public names are spread over them by a fixed, judgment-based table (the public voices cannot be matched by ear): marin maps to maple like the Realtime alias, the rest are spread so neighbouring public names do not share a voice where avoidable, and nira (the public Hindi voice) maps to maple as the voice that suits Hindi best.
// The table is part of the documented behaviour: changing an entry changes what existing clients hear.
var liveSessionVoiceAliases = []liveSessionVoiceAlias{
	{Public: "marin", Live: "maple"},
	{Public: "quartz", Live: "sol"},
	{Public: "ripple", Live: "breeze"},
	{Public: "vesper", Live: "vale"},
	{Public: "willow", Live: "juniper"},
	{Public: "stone", Live: "arbor"},
	{Public: "gleam", Live: "ember"},
	{Public: "meridian", Live: "cove"},
	{Public: "bossa", Live: "spruce"},
	{Public: "tempo", Live: "ember"},
	{Public: "beacon", Live: "sol"},
	{Public: "delta", Live: "arbor"},
	{Public: "cinder", Live: "spruce"},
	{Public: "nuri", Live: "juniper"},
	{Public: "juni", Live: "cove"},
	{Public: "flitz", Live: "breeze"},
	{Public: "brise", Live: "vale"},
	{Public: "sillage", Live: "maple"},
	{Public: "nira", Live: "maple"},
	{Public: "harema", Live: "juniper"},
	{Public: "shida", Live: "vale"},
	{Public: "noeul", Live: "breeze"},
}

// liveSessionVoice resolves a voice name sent in a public GPT-Live session to the ChatGPT Live voice.
// It accepts the public GPT-Live names, the ChatGPT Live names and the OpenAI Realtime names, case-insensitively; an empty name selects the default voice.
func liveSessionVoice(name string) (string, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		name = liveSessionDefaultVoice
	}
	for _, alias := range liveSessionVoiceAliases {
		if alias.Public == name {
			return alias.Live, true
		}
	}
	if liveVoiceNames[name] {
		return name, true
	}
	for _, alias := range realtimeVoiceAliases {
		if alias.OpenAI == name {
			return alias.Live, true
		}
	}
	return "", false
}

// isLiveSessionModel reports whether a model name is accepted by the public GPT-Live surface.
// The upstream model is always liveAudioDefaultModel.
func isLiveSessionModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	return model == "gpt-live-1" || model == liveAudioDefaultModel || isRealtimeAudioModel(model)
}
