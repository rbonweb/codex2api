package admin

import (
	"testing"

	"github.com/codex2api/proxy"
	"github.com/tidwall/gjson"
)

func TestParseCodexDetectorConcurrency(t *testing.T) {
	tests := []struct {
		raw     string
		want    int
		wantErr bool
	}{
		{raw: "", want: 1},
		{raw: " 2 ", want: 2},
		{raw: "3", want: 3},
		{raw: "0", wantErr: true},
		{raw: "4", wantErr: true},
		{raw: "1.5", wantErr: true},
	}
	for _, test := range tests {
		got, err := parseCodexDetectorConcurrency(test.raw)
		if test.wantErr {
			if err == nil {
				t.Errorf("parseCodexDetectorConcurrency(%q) = %d, want error", test.raw, got)
			}
			continue
		}
		if err != nil || got != test.want {
			t.Errorf("parseCodexDetectorConcurrency(%q) = %d, %v; want %d", test.raw, got, err, test.want)
		}
	}
}

func TestParseCodexDetectorSamples(t *testing.T) {
	tests := []struct {
		raw     string
		want    int
		wantErr bool
	}{
		{raw: "", want: proxy.ModelTraceTargetOutputs},
		{raw: " 1 ", want: 1},
		{raw: "2", want: 2},
		{raw: "3", want: 3},
		{raw: "0", wantErr: true},
		{raw: "4", wantErr: true},
		{raw: "x", wantErr: true},
	}
	for _, test := range tests {
		got, err := parseCodexDetectorSamples(test.raw)
		if test.wantErr {
			if err == nil {
				t.Errorf("parseCodexDetectorSamples(%q) = %d, want error", test.raw, got)
			}
			continue
		}
		if err != nil || got != test.want {
			t.Errorf("parseCodexDetectorSamples(%q) = %d, %v; want %d", test.raw, got, err, test.want)
		}
	}
}

func gjsonString(body []byte, path string) string {
	return gjson.GetBytes(body, path).String()
}
