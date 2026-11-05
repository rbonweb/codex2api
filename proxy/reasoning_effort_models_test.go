package proxy

import "testing"

// The reasoning effort aliases in settings are also gated per model for max: 5.6+ keeps it, older models are clamped to xhigh.
func TestParseReasoningEffortModelEntries_MaxGatedByModel(t *testing.T) {
	supported := []string{"gpt-5.6-sol", "gpt-5.4"}
	entries, err := parseReasoningEffortModelEntries(
		`[{"model":"gpt-5.6-sol","effort":"max"},{"model":"gpt-5.4","effort":"max"}]`,
		supported, true)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2: %+v", len(entries), entries)
	}
	if entries[0].Effort != "max" {
		t.Fatalf("gpt-5.6-sol effort = %q, want max", entries[0].Effort)
	}
	if entries[1].Effort != "xhigh" {
		t.Fatalf("gpt-5.4 effort = %q, want xhigh", entries[1].Effort)
	}
}
