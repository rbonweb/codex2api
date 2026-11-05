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

// buildLiveAudioSession returns the session.update body sent to ChatGPT Live when a call is created.
func buildLiveAudioSession(model, voice, instructions string) json.RawMessage {
	if strings.TrimSpace(instructions) == "" {
		instructions = liveAudioDefaultInstructions
	}
	raw, _ := json.Marshal(map[string]any{
		"model":        mapRealtimeModelToLive(model),
		"instructions": instructions,
		"audio": map[string]any{
			"output": map[string]any{"voice": mapRealtimeVoiceToLive(voice)},
		},
		"delegation": map[string]any{"type": "client"},
	})
	return raw
}
