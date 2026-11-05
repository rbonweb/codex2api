package proxy

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/codex2api/auth"
)

// Codex account timezone binding: the timezone and date in the <environment_context> that the client generates in the outbound request body
// are rewritten to constant account-level values. When several people share one account, each downstream client generates them from its own machine,
// <timezone>/<current_date>, so upstream sees the same account jump between time zones; after binding, each account only
// presents one timezone. This is prompt body text rather than a request header, so it is
// independent of device fingerprint convergence (codex_fingerprint.go): binding takes effect immediately and does not depend on the convergence tier.
//
// Rewrite rules:
//   - Only rewrite tags that already exist, never add new ones; if the request body has no environment_context, return it unchanged.
//   - <current_date> is not simply replaced with "today in the account timezone": the environment_context of earlier turns
//     is resent together with the full history (store:false), so changing every old date to today would tamper with history. Here,
//     the whole block shifts by the day difference between "today in the account timezone" and "today in the client timezone", so old blocks keep their relative offsets, and within the
//     same request, new blocks land exactly on today in the account timezone. If the client timezone is missing or cannot be loaded, only the timezone is changed, not the date.
//   - Idempotent: rewriting an already-rewritten value gives the same result, so when the retry path sends a rewritten payload back in, nothing drifts.

const (
	environmentContextOpenTag = "<environment_context>"
	environmentDateLayout     = "2006-01-02"
)

var (
	environmentTimezoneTagPattern = regexp.MustCompile(`<timezone>([^<]*)</timezone>`)
	environmentDateTagPattern     = regexp.MustCompile(`<current_date>(\d{4}-\d{2}-\d{2})</current_date>`)

	// time.LoadLocation reads zoneinfo on every call, so the parsed location is cached on the hot path.
	environmentLocationCache sync.Map // string -> *time.Location
)

func loadLocationCached(name string) *time.Location {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	if cached, ok := environmentLocationCache.Load(name); ok {
		loc, _ := cached.(*time.Location)
		return loc
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		// Failed results are also cached, to avoid bad values repeatedly triggering disk reads.
		environmentLocationCache.Store(name, (*time.Location)(nil))
		return nil
	}
	environmentLocationCache.Store(name, loc)
	return loc
}

// ApplyCodexTimezoneToBody rewrites every environment_context in the request body input according to the account-bound timezone
// text. If the account has no bound timezone, or the request body has no environment_context, it returns unchanged.
func ApplyCodexTimezoneToBody(body []byte, account *auth.Account, now time.Time) []byte {
	timezone := account.EffectiveCodexTimezone()
	if timezone == "" || len(body) == 0 {
		return body
	}
	loc := loadLocationCached(timezone)
	if loc == nil {
		return body
	}
	// Do not pre-check for "<environment_context>" on the raw bytes: the prepare stage has already re-encoded
	// the request body with encoding/json, so "<" became \u003c. Only check the text after gjson decoding.
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body
	}
	accountToday := now.In(loc)
	input.ForEach(func(itemIndex, item gjson.Result) bool {
		content := item.Get("content")
		switch {
		case content.Type == gjson.String:
			if rewritten, changed := rewriteEnvironmentContextText(content.String(), timezone, accountToday); changed {
				body = setJSONStringAtPath(body, fmt.Sprintf("input.%d.content", itemIndex.Int()), rewritten)
			}
		case content.IsArray():
			content.ForEach(func(partIndex, part gjson.Result) bool {
				text := part.Get("text")
				if text.Type != gjson.String {
					return true
				}
				if rewritten, changed := rewriteEnvironmentContextText(text.String(), timezone, accountToday); changed {
					body = setJSONStringAtPath(body, fmt.Sprintf("input.%d.content.%d.text", itemIndex.Int(), partIndex.Int()), rewritten)
				}
				return true
			})
		}
		return true
	})
	return body
}

func setJSONStringAtPath(body []byte, path, value string) []byte {
	updated, err := sjson.SetBytes(body, path, value)
	if err != nil {
		return body
	}
	return updated
}

// rewriteEnvironmentContextText rewrites the timezone and date tags in a piece of text. If the text does not contain
// environment_context, it is left alone; only when it does is the tags scanned (one text may embed several copies, for example
// a historical rollout referenced in the memory-consolidation prompt, rewritten together to keep them consistent).
func rewriteEnvironmentContextText(text, timezone string, accountToday time.Time) (string, bool) {
	if !strings.Contains(text, environmentContextOpenTag) {
		return text, false
	}
	changed := false

	// The client timezone is taken from the first <timezone> in the text; the day shift is computed only when the client timezone can be loaded.
	dayDelta, haveDelta := 0, false
	if match := environmentTimezoneTagPattern.FindStringSubmatch(text); match != nil {
		if clientLoc := loadLocationCached(match[1]); clientLoc != nil {
			clientToday := truncateToDay(accountToday.In(clientLoc))
			dayDelta = int(truncateToDay(accountToday).Sub(clientToday).Hours() / 24)
			haveDelta = true
		}
	}

	rewritten := environmentTimezoneTagPattern.ReplaceAllStringFunc(text, func(tag string) string {
		if environmentTimezoneTagPattern.FindStringSubmatch(tag)[1] == timezone {
			return tag
		}
		changed = true
		return "<timezone>" + timezone + "</timezone>"
	})
	if haveDelta && dayDelta != 0 {
		rewritten = environmentDateTagPattern.ReplaceAllStringFunc(rewritten, func(tag string) string {
			raw := environmentDateTagPattern.FindStringSubmatch(tag)[1]
			parsed, err := time.ParseInLocation(environmentDateLayout, raw, time.UTC)
			if err != nil {
				return tag
			}
			changed = true
			return "<current_date>" + parsed.AddDate(0, 0, dayDelta).Format(environmentDateLayout) + "</current_date>"
		})
	}
	return rewritten, changed
}

// truncateToDay truncates the instant to midnight of its own timezone's day, then converts it to UTC so that subtraction gives whole-day differences.
func truncateToDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}
