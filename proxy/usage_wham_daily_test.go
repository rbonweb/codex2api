package proxy

import (
	"encoding/json"
	"testing"
	"time"
)

// The 2026-08 shape of the current day's record carries no token fields. Pointers tell "upstream did not provide it" apart from "provided 0",
// otherwise an unsettled day would be written as 0 tokens and could not be recognized in later backfills.
// (Since 2026-09 the current-day row has tokens but no models; settlement judgment is in TestWhamDailyUsageDaySettledOnUsesDate.)
func TestWhamDailyUsageUnsettledDayHasNoTokenFields(t *testing.T) {
	raw := `{"data":[
		{"date":"2026-08-10","totals":{"users":1,"threads":289,"turns":1179,"credits":41921.64596825,
			"uncached_text_input_tokens":168988289,"cached_text_input_tokens":1624943616,
			"text_output_tokens":9987798,"text_total_tokens":1803919703},
		 "clients":[{"client_id":"CODEX_CLI","users":1,"threads":88,"turns":501,"credits":10348.5,"text_total_tokens":387827151}],
		 "models":[{"model":"gpt-5.6-sol","credits":0.0,"users":1,"threads":145,"turns":730}]},
		{"date":"2026-08-11","totals":{"users":1,"threads":8,"turns":12,"credits":0.0},
		 "clients":[{"client_id":"CODEX_CLI","users":1,"threads":4,"turns":4,"credits":0.0}],
		 "models":[]}
	],"group_by":"day"}`

	var parsed WhamDailyUsageResponse
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(parsed.Data) != 2 {
		t.Fatalf("expected 2 days, got %d", len(parsed.Data))
	}

	settled := parsed.Data[0]
	if !settled.Totals.HasTokenDetail() {
		t.Fatal("settled day reported without token detail")
	}
	if got := *settled.Totals.TextTotalTokens; got != 1803919703 {
		t.Fatalf("settled total tokens = %d", got)
	}
	// 1 USD = 25 credits.
	if usd := settled.Totals.USD(); usd < 1676.8 || usd > 1676.9 {
		t.Fatalf("settled usd = %f, want ~1676.87", usd)
	}
	if len(settled.Clients) != 1 || settled.Clients[0].ClientID != "CODEX_CLI" {
		t.Fatalf("client split not parsed: %#v", settled.Clients)
	}
	// The counter fields of the embedded struct must be decoded together with the client, otherwise the breakdown table is all zeros.
	if settled.Clients[0].Turns != 501 {
		t.Fatalf("client turns = %d, want 501", settled.Clients[0].Turns)
	}
	if len(settled.Models) != 1 || settled.Models[0].Model != "gpt-5.6-sol" {
		t.Fatalf("model split not parsed: %#v", settled.Models)
	}

	unsettled := parsed.Data[1]
	if unsettled.Totals.HasTokenDetail() {
		t.Fatal("unsettled day reported with token detail")
	}
	if unsettled.Totals.TextTotalTokens != nil {
		t.Fatal("unsettled day must not carry token counts")
	}
	if unsettled.Totals.Turns != 12 {
		t.Fatalf("unsettled turns = %d, want 12", unsettled.Totals.Turns)
	}
}

// The steady-state rolling window is fixed at 7 days (including today): today's and the previous few days' values change, so whole-window overwrite also fills gaps.
func TestWhamDailyUsageWindowCoversRetention(t *testing.T) {
	now := time.Date(2026, 8, 11, 15, 4, 5, 0, time.UTC)
	start, end := WhamDailyUsageWindow(now)
	if end != "2026-08-11" {
		t.Fatalf("end = %s, want 2026-08-11", end)
	}
	if start != "2026-08-05" {
		t.Fatalf("start = %s, want 2026-08-05 (7-day window inclusive)", start)
	}
}
