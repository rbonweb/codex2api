package proxy

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode"

	"github.com/abadojack/whatlanggo"
)

// This file holds the transcript script helpers of the voice surfaces: the transcription language option and the Devanagari to Gujarati conversion.

const (
	// devanagariFirst and devanagariLast bound the Devanagari code points that have a parallel in the Gujarati block.
	// The two Unicode blocks are laid out in parallel: a Devanagari code point plus gujaratiOffset is the matching Gujarati one.
	devanagariFirst = 0x0900
	devanagariLast  = 0x0970
	gujaratiOffset  = 0x180
	// devanagariDanda and devanagariDoubleDanda have no Gujarati counterpart: Gujarati text uses the Devanagari ones, so they stay.
	devanagariDanda       = 0x0964
	devanagariDoubleDanda = 0x0965
)

// devanagariToGujaratiRune maps one Devanagari code point to the Gujarati code point at the same position in its block.
// Only code points that exist in the Gujarati block are mapped: unassigned positions (for example the Devanagari short E and the nukta letters), the danda signs and everything outside U+0900 to U+0970 are returned unchanged, so no unassigned code point is ever produced.
// The Devanagari digits U+0966 to U+096F become the Gujarati digits U+0AE6 to U+0AEF.
func devanagariToGujaratiRune(r rune) rune {
	if r < devanagariFirst || r > devanagariLast || r == devanagariDanda || r == devanagariDoubleDanda {
		return r
	}
	target := r + gujaratiOffset
	if !unicode.Is(unicode.Gujarati, target) {
		return r
	}
	return target
}

// devanagariToGujarati converts the Devanagari characters of text to Gujarati script and leaves every other character as it is.
// ChatGPT Live transcribes Gujarati speech in Devanagari; the letters, vowel signs, nukta, digits and om of the two scripts correspond one to one, so the conversion is a code point shift.
// It works per character, so a transcript that arrives in pieces converts to the same text as the whole.
func devanagariToGujarati(text string) string {
	if !strings.ContainsFunc(text, func(r rune) bool { return r >= devanagariFirst && r <= devanagariLast }) {
		return text
	}
	return strings.Map(devanagariToGujaratiRune, text)
}

// voiceLanguageCodePattern is an ISO 639-1 code: two letters.
var voiceLanguageCodePattern = regexp.MustCompile(`^[A-Za-z]{2}$`)

// normalizeTranscriptionLanguage validates an ISO 639-1 language code and returns it in lower case.
func normalizeTranscriptionLanguage(code string) (string, bool) {
	code = strings.TrimSpace(code)
	if !voiceLanguageCodePattern.MatchString(code) {
		return "", false
	}
	return strings.ToLower(code), true
}

// transcriptionLanguageName returns the English name of an ISO 639-1 language for the developer message, or the code itself when the detector library does not know it.
func transcriptionLanguageName(code string) string {
	for lang, name := range whatlanggo.Langs {
		if lang.Iso6391() == code {
			return name
		}
	}
	return "the language with ISO 639-1 code " + code
}

// languageScripts describes the languages the gateway knows by their script: the English name, the name in the language itself, the script name and the scripts a model must not write it in.
// The keys are the ISO 639-1 codes of the Unicode script fallback, see scriptLanguages.
var languageScripts = map[string]struct{ name, native, script, avoid string }{
	"hi": {"Hindi", "हिन्दी", "Devanagari", "Urdu"},
	"gu": {"Gujarati", "ગુજરાતી", "Gujarati", "Devanagari or Urdu"},
	"bn": {"Bengali", "বাংলা", "Bengali", "Devanagari or Urdu"},
	"pa": {"Punjabi", "ਪੰਜਾਬੀ", "Gurmukhi", "Devanagari or Urdu (Shahmukhi)"},
	"ta": {"Tamil", "தமிழ்", "Tamil", "Devanagari or Urdu"},
	"te": {"Telugu", "తెలుగు", "Telugu", "Devanagari or Urdu"},
	"kn": {"Kannada", "ಕನ್ನಡ", "Kannada", "Devanagari or Urdu"},
	"ml": {"Malayalam", "മലയാളം", "Malayalam", "Devanagari or Urdu"},
	"or": {"Odia", "ଓଡ଼ିଆ", "Odia", "Devanagari or Urdu"},
	"si": {"Sinhala", "සිංහල", "Sinhala", "Devanagari or Urdu"},
	"th": {"Thai", "ไทย", "Thai", "another script"},
	"ko": {"Korean", "한국어", "Hangul", "another script"},
	"ja": {"Japanese", "日本語", "Japanese", "another script"},
	"he": {"Hebrew", "עברית", "Hebrew", "another script"},
	"el": {"Greek", "Ελληνικά", "Greek", "another script"},
	"ka": {"Georgian", "ქართული", "Georgian", "another script"},
	"hy": {"Armenian", "հայերեն", "Armenian", "another script"},
	"km": {"Khmer", "ខ្មែរ", "Khmer", "another script"},
	"lo": {"Lao", "ລາວ", "Lao", "another script"},
	"my": {"Burmese", "မြန်မာ", "Myanmar", "another script"},
	"am": {"Amharic", "አማርኛ", "Ethiopic", "another script"},
}

// transcriptionLanguageText is the developer hint for a pinned transcription language.
// For the languages with a script of their own it says which script to write, because upstream speech recognition returns Gujarati, Devanagari or Urdu script for the same Gujarati audio.
func transcriptionLanguageText(code string) string {
	if info, ok := languageScripts[code]; ok {
		return "The user speaks " + info.name + " (" + code + "). Write every transcript and reply in " + info.script + " script (" + info.native + "), never in " + info.avoid + " script, unless the user switches language."
	}
	name := transcriptionLanguageName(code)
	return "The user speaks " + name + " (" + code + "). Reply in " + name + " unless the user switches to another language."
}

// transcriptionLanguageItem is the developer message that tells ChatGPT Live which language the user speaks.
func transcriptionLanguageItem(code string) map[string]any {
	return map[string]any{
		"type": "message",
		"role": "developer",
		"content": []map[string]any{
			{"type": "input_text", "text": transcriptionLanguageText(code)},
		},
	}
}

// transcriptionLanguageHintEvent is the same hint as a quiet session.context.append on the commentary channel, to send once right after session.started.
// It returns nil for a language without a script of its own, where the initial developer message is enough.
func transcriptionLanguageHintEvent(code string) []byte {
	if _, ok := languageScripts[code]; !ok {
		return nil
	}
	raw, _ := json.Marshal(map[string]any{
		"type":    "session.context.append",
		"channel": "commentary",
		"content": []map[string]any{{"type": "input_text", "text": transcriptionLanguageText(code)}},
	})
	return raw
}

// transcriptionScriptFix returns the conversion applied to input transcripts for a transcription language: Gujarati converts Devanagari to Gujarati script, every other language keeps the text.
// Arabic-script output (Urdu) cannot be converted this way and is reported as it arrives.
func transcriptionScriptFix(code string) func(string) string {
	if code == "gu" {
		return devanagariToGujarati
	}
	return nil
}

// scriptLanguageTable maps a Unicode script to the language written in it, for scripts that belong to one language (or one language in practice).
// Arabic, Cyrillic, Latin and Han are ambiguous (Arabic, Persian and Urdu share a script, Cyrillic covers Russian, Ukrainian and Bulgarian, Han is Chinese or Japanese), so they are left to the detector.
var scriptLanguageTable = []struct {
	table *unicode.RangeTable
	code  string
}{
	{unicode.Devanagari, "hi"}, {unicode.Gujarati, "gu"}, {unicode.Bengali, "bn"}, {unicode.Gurmukhi, "pa"},
	{unicode.Tamil, "ta"}, {unicode.Telugu, "te"}, {unicode.Kannada, "kn"}, {unicode.Malayalam, "ml"},
	{unicode.Oriya, "or"}, {unicode.Sinhala, "si"}, {unicode.Thai, "th"}, {unicode.Hangul, "ko"},
	{unicode.Hiragana, "ja"}, {unicode.Katakana, "ja"}, {unicode.Hebrew, "he"}, {unicode.Greek, "el"},
	{unicode.Georgian, "ka"}, {unicode.Armenian, "hy"}, {unicode.Khmer, "km"}, {unicode.Lao, "lo"},
	{unicode.Myanmar, "my"}, {unicode.Ethiopic, "am"},
}

// scriptShareNum over scriptShareDen is the share of the letters that must be in one script for the script to name the language (more than half), so a Hindi sentence with a few English words still reads as Hindi and an English sentence with one Hindi word does not.
const (
	scriptShareNum = 1
	scriptShareDen = 2
)

// scriptLanguage names the language of text from its Unicode script alone, or "" when the letters are mostly Latin, Arabic, Cyrillic, Han or mixed.
// It is the fallback for transcripts the detector cannot judge: short text such as "नमस्ते" or text the detector is not sure about.
// Marks (Devanagari vowel signs, for example) count with the letters, digits and punctuation do not. Han letters count for Japanese when the text also holds kana, because Japanese text mixes them.
func scriptLanguage(text string) string {
	counts := map[string]int{}
	total, han := 0, 0
	for _, r := range text {
		if !unicode.IsLetter(r) && !unicode.IsMark(r) {
			continue
		}
		total++
		if unicode.Is(unicode.Han, r) {
			han++
			continue
		}
		for _, entry := range scriptLanguageTable {
			if unicode.Is(entry.table, r) {
				counts[entry.code]++
				break
			}
		}
	}
	if counts["ja"] > 0 {
		counts["ja"] += han
	}
	best, bestCount := "", 0
	for code, n := range counts {
		if n > bestCount || (n == bestCount && code < best) {
			best, bestCount = code, n
		}
	}
	if bestCount == 0 || bestCount*scriptShareDen <= total*scriptShareNum {
		return ""
	}
	return best
}

// transcriptScript names the script of a transcript when it is not the script of the pinned language, so a client can tell: "arab" for Arabic (Urdu) script under a pinned language that is written in another script. Only Devanagari can be converted (to Gujarati); Arabic script is reported, never rewritten.
func transcriptScript(language, text string) string {
	if _, ok := languageScripts[language]; !ok {
		return ""
	}
	arabic, total := 0, 0
	for _, r := range text {
		if !unicode.IsLetter(r) {
			continue
		}
		total++
		if unicode.Is(unicode.Arabic, r) {
			arabic++
		}
	}
	if arabic > 0 && arabic*scriptShareDen > total*scriptShareNum {
		return "arab"
	}
	return ""
}

// liveTranscriptLanguage detects the language of a transcript for any language the detector knows (about 84, no list of its own to limit them).
// It returns the ISO 639-1 code, or the ISO 639-3 code when the language has no 639-1 code. The detector decides when the text has at least 3 words and it is reliable; otherwise the Unicode script names the language when the letters are mostly one script of scriptLanguageTable, and the result is "" for everything else.
// Languages written without spaces (Chinese, Japanese, Thai and similar) count two letters as a word. Romanized text is detected by its letters, so romanized Hindi usually reads as English: this is a heuristic, which is why the field is an extension.
func liveTranscriptLanguage(text string) string {
	words, unspaced := len(strings.Fields(text)), 0
	for _, r := range text {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Thai, r) || unicode.Is(unicode.Lao, r) || unicode.Is(unicode.Khmer, r) || unicode.Is(unicode.Myanmar, r) {
			unspaced++
		}
	}
	if unspaced/2 > words {
		words = unspaced / 2
	}
	if words >= 3 {
		if info := whatlanggo.Detect(text); info.IsReliable() {
			if code := info.Lang.Iso6391(); code != "" {
				return code
			}
			return info.Lang.Iso6393()
		}
	}
	return scriptLanguage(text)
}
