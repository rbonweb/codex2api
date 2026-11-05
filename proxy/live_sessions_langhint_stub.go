package proxy

import "encoding/json"

// TEMPORARY STUB: feat/audio-v4-lang defines transcriptionLanguageHintEvent in proxy/audio_script.go. Delete this file when both branches are merged.
func transcriptionLanguageHintEvent(code string) []byte {
	item := transcriptionLanguageItem(code)
	parts, _ := item["content"].([]map[string]any)
	if len(parts) == 0 {
		return nil
	}
	raw, _ := json.Marshal(map[string]any{
		"type":    "session.context.append",
		"channel": "commentary",
		"content": []map[string]any{{"type": "input_text", "text": parts[0]["text"]}},
	})
	return raw
}
