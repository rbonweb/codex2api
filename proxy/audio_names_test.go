package proxy

import (
	"encoding/json"
	"testing"

	"github.com/tidwall/gjson"
)

func TestAudioNamesModelDetection(t *testing.T) {
	tests := []struct {
		model string
		want  bool
	}{
		{"gpt-realtime", true},
		{"gpt-realtime-mini", true},
		{"  GPT-Realtime-2025-08-28  ", true},
		{"gpt-4o-realtime-preview", true},
		{"gpt-4o-realtime-preview-2024-12-17", true},
		{"gpt-4o-mini-realtime-preview", true},
		{"gpt-4o-mini-realtime-preview-2024-12-17", true},
		{"gpt-live", true},
		{"gpt-live-1", true},
		{"gpt-live-1-codex", true},
		{"", false},
		{"   ", false},
		{"gpt-5", false},
		{"gpt-4o", false},
		{"gpt-4o-mini", false},
		{"gpt-4o-audio-preview", false},
		{"gpt-realtimex", false},
		{"gpt-livex", false},
	}
	for _, tt := range tests {
		if got := isRealtimeAudioModel(tt.model); got != tt.want {
			t.Errorf("isRealtimeAudioModel(%q) = %v, want %v", tt.model, got, tt.want)
		}
	}
}

func TestAudioNamesModelMapping(t *testing.T) {
	tests := []struct {
		model string
		want  string
	}{
		{"gpt-realtime", liveAudioDefaultModel},
		{"gpt-realtime-mini", liveAudioDefaultModel},
		{"gpt-4o-realtime-preview", liveAudioDefaultModel},
		{"gpt-live-1", liveAudioDefaultModel},
		{"gpt-live-1-codex", "gpt-live-1-codex"},
		{"GPT-LIVE-1-CODEX", "gpt-live-1-codex"},
		{"", liveAudioDefaultModel},
	}
	for _, tt := range tests {
		if got := mapRealtimeModelToLive(tt.model); got != tt.want {
			t.Errorf("mapRealtimeModelToLive(%q) = %q, want %q", tt.model, got, tt.want)
		}
	}
}

func TestAudioNamesVoiceMapping(t *testing.T) {
	if len(realtimeVoiceAliases) != 10 {
		t.Fatalf("realtimeVoiceAliases has %d entries, want 10", len(realtimeVoiceAliases))
	}
	if realtimeVoiceAliases[0].OpenAI != "alloy" || realtimeVoiceAliases[0].Live != "cove" {
		t.Fatalf("realtimeVoiceAliases must start with alloy->cove, got %+v", realtimeVoiceAliases[0])
	}
	for _, alias := range realtimeVoiceAliases {
		if !liveVoiceNames[alias.Live] {
			t.Errorf("alias %s maps to %q, which is not a Live voice", alias.OpenAI, alias.Live)
		}
		if got := mapRealtimeVoiceToLive(alias.OpenAI); got != alias.Live {
			t.Errorf("mapRealtimeVoiceToLive(%q) = %q, want %q", alias.OpenAI, got, alias.Live)
		}
	}

	tests := []struct {
		voice string
		want  string
	}{
		{"arbor", "arbor"},
		{"breeze", "breeze"},
		{"cove", "cove"},
		{"ember", "ember"},
		{"juniper", "juniper"},
		{"maple", "maple"},
		{"sol", "sol"},
		{"spruce", "spruce"},
		{"vale", "vale"},
		{"  Sol  ", "sol"},
		{"ALLOY", "cove"},
		{"", liveAudioDefaultVoice},
		{"   ", liveAudioDefaultVoice},
		{"unknown-voice", liveAudioDefaultVoice},
	}
	for _, tt := range tests {
		if got := mapRealtimeVoiceToLive(tt.voice); got != tt.want {
			t.Errorf("mapRealtimeVoiceToLive(%q) = %q, want %q", tt.voice, got, tt.want)
		}
	}
}

func TestLiveAudioSessionJSON(t *testing.T) {
	tests := []struct {
		name         string
		model        string
		voice        string
		instructions string
		wantInstr    string
		wantVoice    string
	}{
		{"defaults", "", "", "", liveAudioDefaultInstructions, liveAudioDefaultVoice},
		{"openai voice and model", "gpt-realtime", "alloy", "Be brief.", "Be brief.", "cove"},
		{"blank instructions use default", "gpt-live", "sol", "   ", liveAudioDefaultInstructions, "sol"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := buildLiveAudioSession(tt.model, tt.voice, tt.instructions)
			var got map[string]any
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("buildLiveAudioSession returned invalid JSON: %v", err)
			}
			if got["model"] != liveAudioDefaultModel {
				t.Errorf("model = %v, want %s", got["model"], liveAudioDefaultModel)
			}
			if got["instructions"] != tt.wantInstr {
				t.Errorf("instructions = %v, want %q", got["instructions"], tt.wantInstr)
			}
			audio, _ := got["audio"].(map[string]any)
			output, _ := audio["output"].(map[string]any)
			if output["voice"] != tt.wantVoice {
				t.Errorf("audio.output.voice = %v, want %s", output["voice"], tt.wantVoice)
			}
			delegation, _ := got["delegation"].(map[string]any)
			if delegation["type"] != "client" {
				t.Errorf("delegation.type = %v, want client", delegation["type"])
			}
			parsed := gjson.ParseBytes(raw)
			if parsed.Get("initial_items.#").Int() != 1 || parsed.Get("initial_items.0.type").String() != "message" || parsed.Get("initial_items.0.role").String() != "developer" || parsed.Get("initial_items.0.content.0.type").String() != "input_text" || parsed.Get("initial_items.0.content.0.text").String() != tt.wantInstr {
				t.Errorf("initial_items = %s, want one developer message with %q", parsed.Get("initial_items").Raw, tt.wantInstr)
			}
		})
	}
}

func TestLiveTranscriptionSessionHasNoInitialItems(t *testing.T) {
	raw := buildLiveTranscriptionSession(transcriptionInstructions + " The audio is in fr.")
	got := gjson.ParseBytes(raw)
	if got.Get("initial_items").Exists() {
		t.Fatalf("transcription session must not carry initial_items: %s", raw)
	}
	if got.Get("instructions").String() != transcriptionInstructions+" The audio is in fr." || got.Get("model").String() != liveAudioDefaultModel || got.Get("delegation.type").String() != "client" {
		t.Fatalf("transcription session = %s", raw)
	}
}

func TestAudioNamesLiveSessionVoices(t *testing.T) {
	public := []string{"marin", "quartz", "ripple", "vesper", "willow", "stone", "gleam", "meridian", "bossa", "tempo", "beacon", "delta", "cinder", "nuri", "juni", "flitz", "brise", "sillage", "nira", "harema", "shida", "noeul"}
	if len(liveSessionVoiceAliases) != len(public) {
		t.Fatalf("liveSessionVoiceAliases has %d entries, want %d", len(liveSessionVoiceAliases), len(public))
	}
	for i, name := range public {
		if liveSessionVoiceAliases[i].Public != name {
			t.Errorf("entry %d is %q, want %q", i, liveSessionVoiceAliases[i].Public, name)
		}
		got, ok := liveSessionVoice(name)
		if !ok || !liveVoiceNames[got] {
			t.Errorf("liveSessionVoice(%q) = %q, %v, want an internal voice", name, got, ok)
		}
	}
	if got, _ := liveSessionVoice("marin"); got != "maple" {
		t.Errorf("marin maps to %q, want maple", got)
	}
	if got := mapRealtimeVoiceToLive("marin"); got != "maple" {
		t.Errorf("Realtime marin alias = %q, want maple", got)
	}
	tests := []struct {
		name string
		want string
		ok   bool
	}{
		{"", "maple", true},
		{"  NIRA ", "maple", true},
		{"sol", "sol", true},
		{"Alloy", "cove", true},
		{"cedar", "arbor", true},
		{"nope", "", false},
	}
	for _, tt := range tests {
		got, ok := liveSessionVoice(tt.name)
		if got != tt.want || ok != tt.ok {
			t.Errorf("liveSessionVoice(%q) = %q, %v, want %q, %v", tt.name, got, ok, tt.want, tt.ok)
		}
	}
}

func TestAudioNamesLiveSessionModels(t *testing.T) {
	for _, model := range []string{"gpt-live-1", "gpt-live-1-codex", " GPT-Live-1 ", "gpt-realtime", "gpt-4o-realtime-preview"} {
		if !isLiveSessionModel(model) {
			t.Errorf("isLiveSessionModel(%q) = false, want true", model)
		}
	}
	for _, model := range []string{"", "gpt-5", "gpt-livex", "whisper-1"} {
		if isLiveSessionModel(model) {
			t.Errorf("isLiveSessionModel(%q) = true, want false", model)
		}
	}
}
