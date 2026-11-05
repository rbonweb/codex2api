package proxy

import (
	"net/http"
	"sort"

	"github.com/gin-gonic/gin"
)

// audioVoiceEntry is one accepted voice name in the GET /v1/audio/voices list.
type audioVoiceEntry struct {
	ID string `json:"id"`
	// Object is always "voice".
	Object string `json:"object"`
	// Groups names the families that accept the name: "openai" (Realtime voices), "gpt-live" (public GPT-Live voices) and "live" (native ChatGPT Live voices). A name can be in more than one, such as marin.
	Groups []string `json:"groups"`
	// LiveVoice is the ChatGPT Live voice the call actually uses.
	LiveVoice string `json:"live_voice"`
}

// audioVoiceEntries lists every accepted voice name once, in a fixed order: OpenAI voices, public GPT-Live voices, then native Live voices sorted by name.
// The Live voice of each entry is resolved with liveSessionVoice, the function every audio path accepts names through, and the names come from the alias tables, so the list cannot drift from them.
func audioVoiceEntries() []audioVoiceEntry {
	var entries []audioVoiceEntry
	index := map[string]int{}
	add := func(name, group string) {
		if i, ok := index[name]; ok {
			entries[i].Groups = append(entries[i].Groups, group)
			return
		}
		live, _ := liveSessionVoice(name)
		index[name] = len(entries)
		entries = append(entries, audioVoiceEntry{ID: name, Object: "voice", Groups: []string{group}, LiveVoice: live})
	}
	for _, alias := range realtimeVoiceAliases {
		add(alias.OpenAI, "openai")
	}
	for _, alias := range liveSessionVoiceAliases {
		add(alias.Public, "gpt-live")
	}
	native := make([]string, 0, len(liveVoiceNames))
	for name := range liveVoiceNames {
		native = append(native, name)
	}
	sort.Strings(native)
	for _, name := range native {
		add(name, "live")
	}
	return entries
}

// AudioVoices answers GET /v1/audio/voices and GET /v1/live/voices with every voice name the audio endpoints accept.
// A voice is fixed when a call is created: ChatGPT Live refuses to change it afterwards with immutable_field_update.
// defaults.realtime is the voice /v1/realtime and /v1/audio/speech use when the request names none, defaults.live_sessions the one /v1/live/sessions uses.
func (h *Handler) AudioVoices(c *gin.Context) {
	sessionLive, _ := liveSessionVoice(liveSessionDefaultVoice)
	c.JSON(http.StatusOK, gin.H{
		"object": "list",
		"data":   audioVoiceEntries(),
		"defaults": gin.H{
			"realtime":      gin.H{"voice": liveAudioDefaultVoice, "live_voice": liveAudioDefaultVoice},
			"live_sessions": gin.H{"voice": liveSessionDefaultVoice, "live_voice": sessionLive},
		},
	})
}
