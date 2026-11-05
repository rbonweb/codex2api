package proxy

import (
	"strings"
	"testing"

)

const clientScaffoldingSample = "<system-reminder>\n" +
	"# Environment\n" +
	"You have been invoked in the following environment:\n" +
	" - Primary working directory: C:\\Users\\frank\n" +
	" - Is a git repository: false\n" +
	" - Platform: win32\n" +
	" - Shell: bash\n" +
	"</system-reminder>"

// claudeCodeScaffoldedTurn builds one turn of a Claude Code conversation in the
// shape New-API forwards to codex2api: a Claude Code system prompt plus
// <system-reminder> scaffolding in front of the user's real message.
//
// gitRepo is deliberately a parameter: the environment block carries volatile
// session state (git status, working directory) that changes as the session
// moves, which is exactly what must not reach a cache key.
func claudeCodeScaffoldedTurn(gitRepo, question, history string) []byte {
	return []byte(`{
		"model":"gemini-3.7-flash-high",
		"instructions":"You are Claude Code, Anthropic's official CLI tool for Claude.",
		"input":[
			{"role":"user","content":[
				{"type":"input_text","text":"<system-reminder>\n# Environment\nYou have been invoked in the following environment:\n - Platform: win32\n - Is a git repository: ` + gitRepo + `\n</system-reminder>"},
				{"type":"input_text","text":"<system-reminder>\nYou are powered by the model gemini-3.8-flash-high.\n</system-reminder>"},
				{"type":"input_text","text":"` + question + `"}
			]}` + history + `
		]
	}`)
}

func TestNormalizeClientScaffoldingStripsHarnessScaffolding(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "environment reminder",
			in:   clientScaffoldingSample,
			want: "",
		},
		{
			name: "powered by reminder",
			in:   "<system-reminder>\nYou are powered by the model gemini-3.8-flash-high.\n</system-reminder>",
			want: "",
		},
		{
			name: "agent types reminder",
			in:   "<system-reminder>\nAvailable agent types for the Agent tool:\n- Explore: Read-only search agent\n</system-reminder>",
			want: "",
		},
		{
			name: "skills reminder",
			in:   "<system-reminder>\nThe following skills are available for use with the Skill tool:\n- init: Initialize a new CLAUDE.md file\n</system-reminder>",
			want: "",
		},
		{
			name: "attribution reminder",
			in:   "<system-reminder>\nAttribution for git commits and pull requests you create from here on:\n- End git commit messages with:\nCo-Authored-By: Claude Code <noreply@anthropic.com>\n</system-reminder>",
			want: "",
		},
		{
			name: "standalone attribution line",
			in:   "fix the parser\n\nCo-Authored-By: Claude Code <noreply@anthropic.com>\n",
			want: "fix the parser",
		},
		{
			name: "standalone generated-with line",
			in:   "fix the parser\n\n🤖 Generated with [Claude Code](https://claude.com/claude-code)\n",
			want: "fix the parser",
		},
		{
			name: "standalone powered by line",
			in:   "You are powered by the model gemini-3.8-flash-high.",
			want: "",
		},
		{
			name: "reminders followed by the real question",
			in:   clientScaffoldingSample + "\n\n仔细研究下 token 策略",
			want: "仔细研究下 token 策略",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeClientScaffolding(tc.in); got != tc.want {
				t.Fatalf("normalize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// A question that merely quotes a reminder tag must survive: block removal is
// gated on a scaffolding marker, not on the tag alone.
func TestNormalizeClientScaffoldingKeepsReminderWithoutMarkers(t *testing.T) {
	in := "<system-reminder>remember to run the tests before pushing</system-reminder>"
	if got := normalizeClientScaffolding(in); got != in {
		t.Fatalf("marker-free reminder was modified: %q", got)
	}
}

func TestNormalizeClientScaffoldingIsDeterministicAndIdempotent(t *testing.T) {
	in := clientScaffoldingSample + "\n\n" + "<system-reminder>\nYou are powered by the model gemini-3.8-flash-high.\n</system-reminder>" + "\n\nkeep me"
	first := normalizeClientScaffolding(in)
	if second := normalizeClientScaffolding(in); second != first {
		t.Fatalf("not deterministic: %q vs %q", first, second)
	}
	if twice := normalizeClientScaffolding(first); twice != first {
		t.Fatalf("not idempotent: %q -> %q", first, twice)
	}
	if first != "keep me" {
		t.Fatalf("unexpected result: %q", first)
	}
}

func TestNormalizeClientScaffoldingRespectsEnvToggles(t *testing.T) {
	t.Run("master switch off leaves text untouched", func(t *testing.T) {
		t.Setenv(clientScaffoldingStripEnv, "off")
		if got := normalizeClientScaffolding(clientScaffoldingSample); got != clientScaffoldingSample {
			t.Fatalf("disabled neutralizer modified text: %q", got)
		}
	})
	t.Run("reminder switch off keeps blocks but drops attribution lines", func(t *testing.T) {
		t.Setenv(clientScaffoldingStripRemindersEnv, "false")
		in := clientScaffoldingSample + "\n\nCo-Authored-By: Claude Code <noreply@anthropic.com>\n"
		got := normalizeClientScaffolding(in)
		if !strings.Contains(got, "<system-reminder>") {
			t.Fatalf("reminder block should survive when only the reminder switch is off: %q", got)
		}
		if strings.Contains(got, "Co-Authored-By") {
			t.Fatalf("attribution line should still be dropped: %q", got)
		}
	})
}

// deriveContentSessionSeed normalizes content.Raw, which embeds base64 image
// bytes verbatim, so an image turn pushes megabytes of payload through this text
// pass. Four regexes across that payload cost hundreds of milliseconds per
// request — enough to make the image pipeline test miss its own 45s deadline
// under -race — so the pass is bounded.
func TestNormalizeClientScaffoldingSkipsOversizedPayloads(t *testing.T) {
	oversized := `[{"type":"input_text","text":"` + clientScaffoldingSample + `"},{"type":"input_image","image_url":"data:image/png;base64,` +
		strings.Repeat("A", clientScaffoldingMaxNormalizeBytes) + `"}]`
	if len(oversized) <= clientScaffoldingMaxNormalizeBytes {
		t.Fatalf("fixture is %d bytes, needs to exceed the %d cap", len(oversized), clientScaffoldingMaxNormalizeBytes)
	}
	if got := normalizeClientScaffolding(oversized); got != oversized {
		t.Fatal("text past the cap must be returned unchanged")
	}

	// Under the cap the pass still runs, so the bound is a size limit and not a
	// silent off switch.
	if got := normalizeClientScaffolding("keep me\n\n" + clientScaffoldingSample); got != "keep me" {
		t.Fatalf("scaffolding under the cap must still be stripped, got %q", got)
	}
}




// The affinity key is resolved from the raw body, before the converter runs, so
// the ingress neutralizer cannot protect it. It has to ignore the same
// scaffolding, otherwise a volatile environment block moves the conversation to
// another account every turn and the upstream prompt cache is lost.
func TestDeriveContentSessionSeedIgnoresVolatileClientScaffolding(t *testing.T) {
	first := claudeCodeScaffoldedTurn("false", "帮我写个爬虫", "")
	second := claudeCodeScaffoldedTurn("true", "帮我写个爬虫",
		`,{"role":"assistant","content":[{"type":"output_text","text":"好的"}]},{"role":"user","content":[{"type":"input_text","text":"用 requests"}]}`)

	firstSeed := deriveContentSessionSeed(first)
	secondSeed := deriveContentSessionSeed(second)
	if firstSeed == "" {
		t.Fatal("a scaffolded turn must still anchor a seed")
	}
	if firstSeed != secondSeed {
		t.Fatalf("volatile client scaffolding moved the affinity key: %q vs %q", firstSeed, secondSeed)
	}
}

// Different conversations must still resolve to different affinity keys: the
// neutralizer removes scaffolding, not the user's actual question.
func TestDeriveContentSessionSeedStillDistinguishesScaffoldedConversations(t *testing.T) {
	left := claudeCodeScaffoldedTurn("false", "帮我写个爬虫", "")
	right := claudeCodeScaffoldedTurn("false", "帮我改个爬虫", "")
	if leftSeed, rightSeed := deriveContentSessionSeed(left), deriveContentSessionSeed(right); leftSeed == "" || leftSeed == rightSeed {
		t.Fatalf("different questions must stay isolated: %q vs %q", leftSeed, rightSeed)
	}
}

