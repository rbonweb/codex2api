package proxy

import (
	"strings"
	"testing"
	"unicode"
)

func TestScriptDevanagariToGujaratiTable(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"consonants and vowels", "अआइईउऊएऐओऔ", "અઆઇઈઉઊએઐઓઔ"},
		{"velar to labial consonants", "कखगघङचछजझञटठडढणतथदधनपफबभम", "કખગઘઙચછજઝઞટઠડઢણતથદધનપફબભમ"},
		{"semivowels sibilants and ha", "यरलवशषसह", "યરલવશષસહ"},
		{"ksha and gya ligatures keep their parts", "क्ष ज्ञ", "ક્ષ જ્ઞ"},
		{"vowel signs", "कािीुूृेैोौं ँः", "કાિીુૂૃેૈોૌં ઁઃ"},
		{"nukta", "ड़ ढ़", "ડ઼ ઢ઼"},
		{"om and abbreviation sign", "ॐ ॰", "ૐ ૰"},
		{"digits", "०१२३४५६७८९", "૦૧૨૩૪૫૬૭૮૯"},
		{"candra vowels", "ऑ ॉ ऍ ॅ", "ઑ ૉ ઍ ૅ"},
		{"danda stays", "આ। બ॥", "આ। બ॥"},
		{"unassigned gujarati positions stay devanagari", "ऄ ऎ ऒ ॆ ॊ ऩ ऱ ऴ", "ऄ ऎ ऒ ॆ ॊ ऩ ऱ ऴ"},
		{"precomposed nukta letters stay", "\u0958 \u0959 \u095A \u095B \u095C \u095D \u095E \u095F", "\u0958 \u0959 \u095A \u095B \u095C \u095D \u095E \u095F"},
		{"vedic signs and high spacing dot stay", "॒ ॑ ॱ", "॒ ॑ ॱ"},
		{"latin and punctuation unchanged", "Hello, world 123!", "Hello, world 123!"},
		{"arabic script unchanged", "یہ اردو ہے", "یہ اردو ہے"},
		{"already gujarati unchanged", "ગુજરાતી ભાષા", "ગુજરાતી ભાષા"},
		{"mixed text", "નમસ્તે, તમે કેમ છો? मैं ठीक हूँ।", "નમસ્તે, તમે કેમ છો? મૈં ઠીક હૂઁ।"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := devanagariToGujarati(tc.in); got != tc.want {
				t.Fatalf("devanagariToGujarati(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestScriptDevanagariToGujaratiNeverProducesUnassignedCodePoints(t *testing.T) {
	for r := rune(0x0900); r <= 0x097F; r++ {
		got := devanagariToGujaratiRune(r)
		if got == r {
			continue
		}
		if !unicode.Is(unicode.Gujarati, got) {
			t.Errorf("U+%04X mapped to U+%04X, which is not a Gujarati code point", r, got)
		}
		if got-r != gujaratiOffset {
			t.Errorf("U+%04X mapped to U+%04X, not by the block offset", r, got)
		}
	}
	// Outside the Devanagari block nothing moves.
	for _, r := range []rune{'a', 'Z', '5', 0x0627, 0x0A85, 0x1F600} {
		if got := devanagariToGujaratiRune(r); got != r {
			t.Errorf("U+%04X changed to U+%04X", r, got)
		}
	}
}

func TestScriptDevanagariToGujaratiIsChunkSafe(t *testing.T) {
	whole := "नमस्ते, आप कैसे हैं? ०१२"
	var pieces strings.Builder
	for _, r := range whole {
		pieces.WriteString(devanagariToGujarati(string(r)))
	}
	if pieces.String() != devanagariToGujarati(whole) {
		t.Fatalf("per-character conversion %q differs from whole %q", pieces.String(), devanagariToGujarati(whole))
	}
	if devanagariToGujarati(whole) != "નમસ્તે, આપ કૈસે હૈં? ૦૧૨" {
		t.Fatalf("conversion = %q", devanagariToGujarati(whole))
	}
}

func TestScriptTranscriptionLanguageCodes(t *testing.T) {
	for in, want := range map[string]string{"gu": "gu", "EN": "en", " hi ": "hi"} {
		if got, ok := normalizeTranscriptionLanguage(in); !ok || got != want {
			t.Errorf("normalize(%q) = %q, %v", in, got, ok)
		}
	}
	for _, bad := range []string{"", "g", "gujarati", "en-US", "e1", "g u"} {
		if _, ok := normalizeTranscriptionLanguage(bad); ok {
			t.Errorf("normalize(%q) accepted", bad)
		}
	}
	if got := transcriptionLanguageName("gu"); got != "Gujarati" {
		t.Errorf("name(gu) = %q", got)
	}
	if got := transcriptionLanguageName("zz"); !strings.Contains(got, "zz") {
		t.Errorf("name(zz) = %q", got)
	}
	if transcriptionScriptFix("gu") == nil || transcriptionScriptFix("hi") != nil || transcriptionScriptFix("ur") != nil {
		t.Error("only Gujarati converts the script")
	}
	item := transcriptionLanguageItem("gu")
	text, _ := item["content"].([]map[string]any)[0]["text"].(string)
	if item["role"] != "developer" || !strings.Contains(text, "Gujarati script (ગુજરાતી), never in Devanagari or Urdu script") || !strings.Contains(text, "unless the user switches language") {
		t.Errorf("language item = %v", item)
	}
}

func TestScriptLanguageTable(t *testing.T) {
	cases := []struct{ text, want string }{
		{"नमस्ते", "hi"}, {"आज मौसम कैसा है", "hi"}, {"आज weather कैसा है", "hi"},
		{"આજે હવામાન કેવું છે", "gu"}, {"আজ আবহাওয়া কেমন", "bn"}, {"ਅੱਜ ਮੌਸਮ ਕਿਵੇਂ ਹੈ", "pa"},
		{"இன்று வானிலை எப்படி", "ta"}, {"ఈరోజు వాతావరణం ఎలా ఉంది", "te"}, {"ಇಂದು ಹವಾಮಾನ ಹೇಗಿದೆ", "kn"},
		{"ഇന്ന് കാലാവസ്ഥ എങ്ങനെ", "ml"}, {"ଆଜି ପାଣିପାଗ କେମିତି", "or"}, {"අද කාලගුණය කොහොමද", "si"},
		{"สวัสดี", "th"}, {"안녕하세요", "ko"}, {"こんにちは", "ja"}, {"カタカナ", "ja"}, {"今日はいい天気です", "ja"},
		{"שלום", "he"}, {"Γειά σου", "el"}, {"გამარჯობა", "ka"}, {"Բարեւ", "hy"}, {"សួស្តី", "km"},
		{"ສະບາຍດີ", "lo"}, {"မင်္ဂလာပါ", "my"}, {"ሰላም", "am"},
		// Ambiguous or mostly other scripts: no answer from the script.
		{"مرحبا", ""}, {"привет", ""}, {"hello", ""}, {"你好", ""}, {"12345 !?", ""}, {"", ""},
		{"The meeting is at नमस्ते three", ""},
	}
	for _, tc := range cases {
		if got := scriptLanguage(tc.text); got != tc.want {
			t.Errorf("scriptLanguage(%q) = %q, want %q", tc.text, got, tc.want)
		}
	}
}

func TestLiveTranscriptLanguageFallsBackToScriptForShortText(t *testing.T) {
	cases := []struct{ text, want string }{
		{"नमस्ते", "hi"}, {"आज मौसम कैसा है", "hi"}, {"હેલો", "gu"}, {"こんにちは", "ja"},
		{"Hello", ""}, {"hello there", ""},
		{"Hello, can you please tell me what is the capital of India", "en"},
	}
	for _, tc := range cases {
		if got := liveTranscriptLanguage(tc.text); got != tc.want {
			t.Errorf("liveTranscriptLanguage(%q) = %q, want %q", tc.text, got, tc.want)
		}
	}
}

func TestTranscriptScriptReportsArabicUnderPinnedGujarati(t *testing.T) {
	if got := transcriptScript("gu", "یہ اردو ہے"); got != "arab" {
		t.Errorf("arabic under gu = %q", got)
	}
	for _, tc := range []struct{ lang, text string }{{"gu", "આજે હવામાન"}, {"gu", "આજે હવામાન कैसा"}, {"ur", "یہ اردو ہے"}, {"en", "یہ اردو ہے"}, {"gu", ""}} {
		if got := transcriptScript(tc.lang, tc.text); got != "" {
			t.Errorf("transcriptScript(%q, %q) = %q", tc.lang, tc.text, got)
		}
	}
}

func TestTranscriptionLanguageHintNamesScriptForEveryScriptLanguage(t *testing.T) {
	for code, info := range languageScripts {
		text := transcriptionLanguageText(code)
		if !strings.Contains(text, info.native) || !strings.Contains(text, info.script+" script") || !strings.Contains(text, "unless the user switches language") {
			t.Errorf("%s hint = %q", code, text)
		}
		event := transcriptionLanguageHintEvent(code)
		if !strings.Contains(string(event), `"channel":"commentary"`) || !strings.Contains(string(event), `"type":"session.context.append"`) {
			t.Errorf("%s hint event = %s", code, event)
		}
	}
	if transcriptionLanguageHintEvent("en") != nil || !strings.Contains(transcriptionLanguageText("en"), "Reply in English") {
		t.Error("English keeps the plain hint and has no quiet append")
	}
}
