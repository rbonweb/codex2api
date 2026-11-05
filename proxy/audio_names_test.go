package proxy

import (
	"encoding/json"
	"testing"
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
		})
	}
}
