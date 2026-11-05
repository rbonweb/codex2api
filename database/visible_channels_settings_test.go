package database

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestNormalizeVisibleChannels(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil means everything visible", nil, []string{"codex"}},
		{"empty list keeps the fallback", []string{}, []string{"codex"}},
		{"fallback is added when missing", []string{"other"}, []string{"codex"}},
		{"unknown, blank and duplicate entries are dropped and order is canonical", []string{" Codex ", "", "other", "codex", "unknown"}, []string{"codex"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeVisibleChannels(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("NormalizeVisibleChannels(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestVisibleChannelsConfigRoundTrip(t *testing.T) {
	// An explicit empty list must serialize a channels field, otherwise reading it back would be treated as "never configured" and show all.
	raw, err := json.Marshal(VisibleChannelsConfig{Channels: NormalizeVisibleChannels([]string{})})
	if err != nil {
		t.Fatal(err)
	}
	var cfg VisibleChannelsConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if got := cfg.Effective(); !reflect.DeepEqual(got, []string{"codex"}) {
		t.Fatalf("round trip of explicit fallback-only list = %v, raw=%s", got, raw)
	}
	var missing VisibleChannelsConfig
	if err := json.Unmarshal([]byte(`{}`), &missing); err != nil {
		t.Fatal(err)
	}
	if got := missing.Effective(); len(got) != len(AllUpstreamChannels) {
		t.Fatalf("missing field should mean all visible, got %v", got)
	}
}
