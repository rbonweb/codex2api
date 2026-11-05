package proxy

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
)

type voicesResponse struct {
	Object string `json:"object"`
	Data   []struct {
		ID        string   `json:"id"`
		Groups    []string `json:"groups"`
		LiveVoice string   `json:"live_voice"`
	} `json:"data"`
	Defaults map[string]map[string]string `json:"defaults"`
}

func getVoices(t *testing.T) voicesResponse {
	t.Helper()
	c, rec := liveTestContext(t, http.MethodGet, "/v1/audio/voices", nil, nil)
	(&Handler{}).AudioVoices(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out voicesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAudioVoicesListsEveryAcceptedName(t *testing.T) {
	out := getVoices(t)
	if out.Object != "list" {
		t.Fatalf("object = %q", out.Object)
	}
	seen := map[string]string{}
	for _, v := range out.Data {
		if _, dup := seen[v.ID]; dup {
			t.Fatalf("%s listed twice", v.ID)
		}
		seen[v.ID] = v.LiveVoice
		if !liveVoiceNames[v.LiveVoice] {
			t.Fatalf("%s maps to %q, not a Live voice", v.ID, v.LiveVoice)
		}
		if want, ok := liveSessionVoice(v.ID); !ok || want != v.LiveVoice {
			t.Fatalf("%s: listed %q, resolved %q ok=%v", v.ID, v.LiveVoice, want, ok)
		}
	}
	for _, a := range realtimeVoiceAliases {
		if seen[a.OpenAI] == "" {
			t.Fatalf("OpenAI voice %s missing", a.OpenAI)
		}
	}
	for _, a := range liveSessionVoiceAliases {
		if seen[a.Public] != a.Live {
			t.Fatalf("public voice %s = %q, want %q", a.Public, seen[a.Public], a.Live)
		}
	}
	for name := range liveVoiceNames {
		if seen[name] != name {
			t.Fatalf("native voice %s = %q", name, seen[name])
		}
	}
	// 10 OpenAI + 22 public + 9 native, minus the names shared between the families.
	shared := 0
	for _, v := range out.Data {
		if len(v.Groups) > 1 {
			shared++
		}
	}
	if want := 10 + 22 + 9 - shared; len(out.Data) != want {
		t.Fatalf("listed %d voices, want %d", len(out.Data), want)
	}
	if out.Defaults["realtime"]["voice"] != "cove" || out.Defaults["live_sessions"]["voice"] != "marin" || out.Defaults["live_sessions"]["live_voice"] != "maple" {
		t.Fatalf("defaults = %v", out.Defaults)
	}
}

func TestAudioVoicesMarinIsInBothFamilies(t *testing.T) {
	for _, v := range getVoices(t).Data {
		if v.ID == "marin" {
			if len(v.Groups) != 2 || v.Groups[0] != "openai" || v.Groups[1] != "gpt-live" || v.LiveVoice != "maple" {
				t.Fatalf("marin = %+v", v)
			}
			return
		}
	}
	t.Fatal("marin missing")
}

func TestLiveVoicesAndSpeechRoutesRegistered(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	(&Handler{}).RegisterRoutes(router)
	routes := map[string]bool{}
	for _, route := range router.Routes() {
		routes[route.Method+" "+route.Path] = true
	}
	for _, want := range []string{
		"POST /v1/audio/speech", "POST /audio/speech",
		"GET /v1/audio/voices", "GET /audio/voices",
		"GET /v1/live/voices", "GET /live/voices",
	} {
		if !routes[want] {
			t.Fatalf("route %s is not registered", want)
		}
	}
}
