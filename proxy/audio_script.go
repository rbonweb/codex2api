package proxy

import (
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

// transcriptionLanguageItem is the developer message that tells ChatGPT Live which language the user speaks.
func transcriptionLanguageItem(code string) map[string]any {
	name := transcriptionLanguageName(code)
	text := "The user speaks " + name + " (" + code + "). Reply in " + name + " unless the user switches to another language."
	return map[string]any{
		"type": "message",
		"role": "developer",
		"content": []map[string]any{
			{"type": "input_text", "text": text},
		},
	}
}

// transcriptionScriptFix returns the conversion applied to input transcripts for a transcription language: Gujarati converts Devanagari to Gujarati script, every other language keeps the text.
// Arabic-script output (Urdu) cannot be converted this way and is reported as it arrives.
func transcriptionScriptFix(code string) func(string) string {
	if code == "gu" {
		return devanagariToGujarati
	}
	return nil
}
