package proxy

import (
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/codex2api/auth"
)

func codexTimezoneTestAccount(timezone string) *auth.Account {
	return &auth.Account{DBID: 42, RefreshToken: "rt", Timezone: timezone}
}

const envContextText = "<environment_context>\n  <cwd>/Users/kyx/code_project/codex2api</cwd>\n  <shell>zsh</shell>\n  <current_date>2026-09-09</current_date>\n  <timezone>Asia/Shanghai</timezone>\n  <filesystem></filesystem>\n</environment_context>"

func envContextBody(text string) []byte {
	return []byte(`{"type":"response.create","model":"gpt-5.6-luna","input":[{"type":"message","role":"developer","content":[{"type":"input_text","text":"hello"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":` + quoteJSON(text) + `},{"type":"input_text","text":"do it"}]}],"client_metadata":{"session_id":"s"}}`)
}

func quoteJSON(s string) string {
	b := strings.Builder{}
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// 2026-09-09 04:00 上海 = 2026-09-08 16:00 纽约：账号时区落后客户端一天。
var envContextNow = time.Date(2026, 9, 9, 4, 0, 0, 0, time.FixedZone("CST", 8*3600))

func TestApplyCodexTimezoneToBody_RewritesTimezoneAndShiftsDate(t *testing.T) {
	body := envContextBody(envContextText)
	got := ApplyCodexTimezoneToBody(body, codexTimezoneTestAccount("America/New_York"), envContextNow)
	text := gjson.GetBytes(got, "input.1.content.0.text").String()
	if !strings.Contains(text, "<timezone>America/New_York</timezone>") {
		t.Fatalf("timezone not rewritten: %s", text)
	}
	if !strings.Contains(text, "<current_date>2026-09-08</current_date>") {
		t.Fatalf("date not shifted to account timezone day: %s", text)
	}
	// 其它部分与形状保持不变。
	if gjson.GetBytes(got, "input.1.content.1.text").String() != "do it" {
		t.Fatalf("sibling content changed: %s", got)
	}
	if !strings.Contains(text, "<cwd>/Users/kyx/code_project/codex2api</cwd>") {
		t.Fatalf("unrelated tags changed: %s", text)
	}
	// 幂等：改写后的载荷再过一遍不再变化。
	again := ApplyCodexTimezoneToBody(got, codexTimezoneTestAccount("America/New_York"), envContextNow)
	if string(again) != string(got) {
		t.Fatalf("rewrite is not idempotent:\n%s\n%s", got, again)
	}
}

func TestApplyCodexTimezoneToBody_SameDayOnlyRewritesTimezone(t *testing.T) {
	body := envContextBody(envContextText)
	// 东京与上海同一天，日期不动。
	got := ApplyCodexTimezoneToBody(body, codexTimezoneTestAccount("Asia/Tokyo"), envContextNow)
	text := gjson.GetBytes(got, "input.1.content.0.text").String()
	if !strings.Contains(text, "<timezone>Asia/Tokyo</timezone>") || !strings.Contains(text, "<current_date>2026-09-09</current_date>") {
		t.Fatalf("unexpected rewrite: %s", text)
	}
}

func TestApplyCodexTimezoneToBody_HistoricalBlocksKeepRelativeDates(t *testing.T) {
	old := strings.Replace(envContextText, "2026-09-09", "2026-09-06", 1)
	body := []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":` + quoteJSON(old) + `}]},{"type":"message","role":"user","content":[{"type":"input_text","text":` + quoteJSON(envContextText) + `}]}]}`)
	got := ApplyCodexTimezoneToBody(body, codexTimezoneTestAccount("America/New_York"), envContextNow)
	if first := gjson.GetBytes(got, "input.0.content.0.text").String(); !strings.Contains(first, "<current_date>2026-09-05</current_date>") {
		t.Fatalf("historical block should shift by the same delta: %s", first)
	}
	if second := gjson.GetBytes(got, "input.1.content.0.text").String(); !strings.Contains(second, "<current_date>2026-09-08</current_date>") {
		t.Fatalf("fresh block should land on account-timezone today: %s", second)
	}
}

func TestApplyCodexTimezoneToBody_NoOpCases(t *testing.T) {
	body := envContextBody(envContextText)
	cases := map[string]struct {
		account *auth.Account
		body    []byte
	}{
		"unbound account":  {codexTimezoneTestAccount(""), body},
		"invalid timezone": {codexTimezoneTestAccount("Mars/Olympus"), body},
		"same timezone":    {codexTimezoneTestAccount("Asia/Shanghai"), body},
		"no env context":   {codexTimezoneTestAccount("America/New_York"), []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"plain"}]}]}`)},
		"string input":     {codexTimezoneTestAccount("America/New_York"), []byte(`{"input":"plain <environment_context> text"}`)},
		"nil account":      {nil, body},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := ApplyCodexTimezoneToBody(tc.body, tc.account, envContextNow); string(got) != string(tc.body) {
				t.Fatalf("expected no-op, got:\n%s", got)
			}
		})
	}
}

func TestApplyCodexTimezoneToBody_StringContent(t *testing.T) {
	body := []byte(`{"input":[{"type":"message","role":"user","content":` + quoteJSON(envContextText) + `}]}`)
	got := ApplyCodexTimezoneToBody(body, codexTimezoneTestAccount("Europe/London"), envContextNow)
	text := gjson.GetBytes(got, "input.0.content").String()
	if !strings.Contains(text, "<timezone>Europe/London</timezone>") || !strings.Contains(text, "<current_date>2026-09-08</current_date>") {
		t.Fatalf("string content not rewritten: %s", text)
	}
}

func TestRewriteEnvironmentContextText_UnknownClientTimezoneKeepsDate(t *testing.T) {
	text := strings.Replace(envContextText, "Asia/Shanghai", "Nowhere/Land", 1)
	got, changed := rewriteEnvironmentContextText(text, "America/New_York", envContextNow)
	if !changed || !strings.Contains(got, "<timezone>America/New_York</timezone>") {
		t.Fatalf("timezone should still be rewritten: %s", got)
	}
	if !strings.Contains(got, "<current_date>2026-09-09</current_date>") {
		t.Fatalf("date must stay untouched when the client timezone is unknown: %s", got)
	}
}

func TestApplyCodexTimezoneToBody_HTMLEscapedBody(t *testing.T) {
	// prepare 阶段用 encoding/json 重编请求体，"<" 已成 \u003c；改写必须基于解码后的文本。
	body := []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"\u003cenvironment_context\u003e\n  \u003ccurrent_date\u003e2026-09-09\u003c/current_date\u003e\n  \u003ctimezone\u003eAsia/Shanghai\u003c/timezone\u003e\n\u003c/environment_context\u003e"}]}]}`)
	got := ApplyCodexTimezoneToBody(body, codexTimezoneTestAccount("America/New_York"), envContextNow)
	text := gjson.GetBytes(got, "input.0.content.0.text").String()
	if !strings.Contains(text, "<timezone>America/New_York</timezone>") || !strings.Contains(text, "<current_date>2026-09-08</current_date>") {
		t.Fatalf("escaped body not rewritten: %s", text)
	}
}
