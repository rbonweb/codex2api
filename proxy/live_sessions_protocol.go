package proxy

import (
	"encoding/base64"
	"encoding/json"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/tidwall/gjson"
)

// This file is the transport-free core of the public GPT-Live protocol.
// It validates a public session, builds the internal call-create session, and translates events in both directions.
// It does no I/O and starts no goroutines: a WebSocket transport (the gateway owns the WebRTC peer and the audio) and a WebRTC transport (the client owns the media) both drive it.

const (
	// liveSessionMaxInstructionTokens is the public cap on instructions, estimated as bytes/4.
	liveSessionMaxInstructionTokens = 16384
	// liveSessionMaxInputItems and liveSessionMaxInputTokens cap the conversation history, as Codex does.
	liveSessionMaxInputItems  = 128
	liveSessionMaxInputTokens = 8192
	// liveContextAppendMaxBytes is the largest text chunk the internal sideband accepts in one context append.
	liveContextAppendMaxBytes = 500
	// liveMaxQueuedEvents bounds the sideband events held back before upstream session.started.
	liveMaxQueuedEvents = 256
	// liveLanguageWindow is the most recent transcript text, in bytes, the language detector looks at.
	liveLanguageWindow = 200
	// liveTurnTextMax bounds the transcript kept for the reply in progress.
	liveTurnTextMax = 32768
	// liveUsageSilence is how long upstream may stay silent before the gateway clock reports usage itself.
	liveUsageSilence = 60 * time.Second
)

// liveSessionConfig is a validated public GPT-Live session.
type liveSessionConfig struct {
	Model        string            // public name as sent, default "gpt-live-1"
	Instructions string            // system instructions, immutable after start
	Voice        string            // public voice name, default "marin"
	LiveVoice    string            // internal voice
	FormatType   string            // "audio/pcm" | "audio/pcmu" | "audio/pcma"
	FormatRate   int               // 24000 | 16000 | 8000
	AckFiller    bool              // internal delegation.ack_filler
	AckDelayMS   int               // accepted acknowledgement_delay_ms, echoed only
	Language     string            // gateway extension audio.input.transcription.language, an ISO 639-1 code in lower case, "" when unset
	InitialItems []json.RawMessage // internal initial_items
	Store        *bool
}

// liveSessionError is a public error; Event renders the public error event.
type liveSessionError struct{ Code, Param, Message, ClientEventID string }

func (e *liveSessionError) Error() string { return e.Code + ": " + e.Message }

// Event renders the public error event: {"type":"error","error":{...}} with param and client_event_id null when empty.
func (e *liveSessionError) Event() []byte {
	return liveErrorEvent("invalid_request_error", e.Code, e.Message, e.Param, e.ClientEventID)
}

func liveErrorEvent(errType, code, message, param, clientEventID string) []byte {
	if errType == "" {
		errType = "invalid_request_error"
	}
	var paramValue, clientValue any
	if param != "" {
		paramValue = param
	}
	if clientEventID != "" {
		clientValue = clientEventID
	}
	raw, _ := json.Marshal(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":            errType,
			"code":            code,
			"message":         message,
			"param":           paramValue,
			"client_event_id": clientValue,
		},
	})
	return raw
}

func newLiveSessionError(code, param, message, clientEventID string) *liveSessionError {
	return &liveSessionError{Code: code, Param: param, Message: message, ClientEventID: clientEventID}
}

// liveUnknownKeys returns an unknown_parameter error for the first key of obj (in document order) that is not allowed.
func liveUnknownKeys(obj gjson.Result, param string, allowed []string, clientEventID string) *liveSessionError {
	var found *liveSessionError
	obj.ForEach(func(key, _ gjson.Result) bool {
		name := key.String()
		for _, a := range allowed {
			if a == name {
				return true
			}
		}
		full := param + "." + name
		found = newLiveSessionError("unknown_parameter", full, "Unknown parameter: '"+full+"'.", clientEventID)
		return false
	})
	return found
}

// liveAbsent reports whether a field is missing or null.
func liveAbsent(v gjson.Result) bool { return !v.Exists() || v.Type == gjson.Null }

// liveWholeNumber reads a JSON number that is an integer.
func liveWholeNumber(v gjson.Result) (int, bool) {
	if v.Type != gjson.Number {
		return 0, false
	}
	f := v.Float()
	if f != math.Floor(f) || f > math.MaxInt32 || f < math.MinInt32 {
		return 0, false
	}
	return int(f), true
}

// parseLiveSessionObject validates a public session object strictly (unknown field anywhere in it -> unknown_parameter with the dotted param, e.g. "session.audio.foo").
func parseLiveSessionObject(session gjson.Result, clientEventID string) (*liveSessionConfig, *liveSessionError) {
	fail := func(code, param, msg string) (*liveSessionConfig, *liveSessionError) {
		return nil, newLiveSessionError(code, param, msg, clientEventID)
	}
	if !session.IsObject() {
		return fail("invalid_value", "session", "session must be an object.")
	}

	// Unknown fields first, at every level, so a typo is never reported as a value problem.
	if e := liveUnknownKeys(session, "session", []string{"model", "instructions", "input", "audio", "delegation", "store"}, clientEventID); e != nil {
		return nil, e
	}
	audio := session.Get("audio")
	if !liveAbsent(audio) {
		if !audio.IsObject() {
			return fail("invalid_value", "session.audio", "session.audio must be an object.")
		}
		if e := liveUnknownKeys(audio, "session.audio", []string{"format", "output", "input"}, clientEventID); e != nil {
			return nil, e
		}
		if in := audio.Get("input"); !liveAbsent(in) {
			if !in.IsObject() {
				return fail("invalid_value", "session.audio.input", "session.audio.input must be an object.")
			}
			if e := liveUnknownKeys(in, "session.audio.input", []string{"transcription"}, clientEventID); e != nil {
				return nil, e
			}
			if tr := in.Get("transcription"); !liveAbsent(tr) {
				if !tr.IsObject() {
					return fail("invalid_value", "session.audio.input.transcription", "session.audio.input.transcription must be an object.")
				}
				if e := liveUnknownKeys(tr, "session.audio.input.transcription", []string{"language"}, clientEventID); e != nil {
					return nil, e
				}
			}
		}
		if f := audio.Get("format"); !liveAbsent(f) {
			if !f.IsObject() {
				return fail("invalid_value", "session.audio.format", "session.audio.format must be an object.")
			}
			if e := liveUnknownKeys(f, "session.audio.format", []string{"type", "rate"}, clientEventID); e != nil {
				return nil, e
			}
		}
		if o := audio.Get("output"); !liveAbsent(o) {
			if !o.IsObject() {
				return fail("invalid_value", "session.audio.output", "session.audio.output must be an object.")
			}
			if e := liveUnknownKeys(o, "session.audio.output", []string{"voice"}, clientEventID); e != nil {
				return nil, e
			}
		}
	}
	delegation := session.Get("delegation")
	if !liveAbsent(delegation) {
		if !delegation.IsObject() {
			return fail("invalid_value", "session.delegation", "session.delegation must be an object.")
		}
		if e := liveUnknownKeys(delegation, "session.delegation", []string{"type", "acknowledgement_delay_ms", "acknowledgement", "responses"}, clientEventID); e != nil {
			return nil, e
		}
	}
	if input := session.Get("input"); !liveAbsent(input) && input.IsArray() {
		for i, item := range input.Array() {
			param := "session.input." + liveItoa(i)
			if !item.IsObject() {
				continue
			}
			if e := liveUnknownKeys(item, param, []string{"type", "role", "content"}, clientEventID); e != nil {
				return nil, e
			}
			if content := item.Get("content"); content.IsArray() {
				for j, part := range content.Array() {
					if !part.IsObject() {
						continue
					}
					if e := liveUnknownKeys(part, param+".content."+liveItoa(j), []string{"type", "text"}, clientEventID); e != nil {
						return nil, e
					}
				}
			}
		}
	}

	cfg := &liveSessionConfig{
		Model:      liveSessionDefaultModel,
		Voice:      liveSessionDefaultVoice,
		FormatType: "audio/pcm",
		FormatRate: 24000,
		AckFiller:  true,
	}

	// model
	if m := session.Get("model"); !liveAbsent(m) {
		if m.Type != gjson.String {
			return fail("invalid_value", "session.model", "session.model must be a string.")
		}
		if strings.TrimSpace(m.String()) != "" {
			if !isLiveSessionModel(m.String()) {
				return fail("unsupported_value", "session.model", "Unsupported model: '"+m.String()+"'. Use gpt-live-1.")
			}
			cfg.Model = m.String()
		}
	}

	// instructions
	if v := session.Get("instructions"); !liveAbsent(v) {
		if v.Type != gjson.String {
			return fail("invalid_value", "session.instructions", "session.instructions must be a string.")
		}
		if len(v.String())/4 > liveSessionMaxInstructionTokens {
			return fail("invalid_value", "session.instructions", "session.instructions is longer than 16,384 tokens.")
		}
		cfg.Instructions = v.String()
	}

	// voice
	if v := audio.Get("output.voice"); !liveAbsent(v) {
		if v.Type != gjson.String {
			return fail("invalid_value", "session.audio.output.voice", "session.audio.output.voice must be a string.")
		}
		if strings.TrimSpace(v.String()) != "" {
			live, ok := liveSessionVoice(v.String())
			if !ok {
				return fail("unsupported_value", "session.audio.output.voice", "Unsupported voice: '"+v.String()+"'.")
			}
			cfg.Voice = strings.ToLower(strings.TrimSpace(v.String()))
			cfg.LiveVoice = live
		}
	}
	if cfg.LiveVoice == "" {
		cfg.LiveVoice, _ = liveSessionVoice(cfg.Voice)
	}

	// audio format
	if f := audio.Get("format"); !liveAbsent(f) {
		if t := f.Get("type"); !liveAbsent(t) {
			if t.Type != gjson.String {
				return fail("invalid_value", "session.audio.format.type", "session.audio.format.type must be a string.")
			}
			switch t.String() {
			case "audio/pcm", "audio/pcmu", "audio/pcma":
				cfg.FormatType = t.String()
			default:
				return fail("unsupported_value", "session.audio.format.type", "Unsupported audio format: '"+t.String()+"'. Use audio/pcm, audio/pcmu or audio/pcma.")
			}
			if cfg.FormatType != "audio/pcm" {
				cfg.FormatRate = 8000
			}
		}
		if r := f.Get("rate"); !liveAbsent(r) {
			rate, ok := liveWholeNumber(r)
			valid := ok && (cfg.FormatType == "audio/pcm" && (rate == 24000 || rate == 16000) || cfg.FormatType != "audio/pcm" && rate == 8000)
			if !valid {
				want := "24000 or 16000 for audio/pcm"
				if cfg.FormatType != "audio/pcm" {
					want = "8000 for " + cfg.FormatType
				}
				return fail("unsupported_value", "session.audio.format.rate", "Unsupported sample rate. Use "+want+".")
			}
			cfg.FormatRate = rate
		}
	}

	// transcription language (gateway extension, the shape of OpenAI Realtime GA audio.input.transcription.language)
	if l := audio.Get("input.transcription.language"); !liveAbsent(l) {
		if l.Type != gjson.String {
			return fail("invalid_value", "session.audio.input.transcription.language", "session.audio.input.transcription.language must be a string.")
		}
		code, ok := normalizeTranscriptionLanguage(l.String())
		if !ok {
			return fail("invalid_value", "session.audio.input.transcription.language", "session.audio.input.transcription.language must be an ISO 639-1 code such as en, hi or gu.")
		}
		cfg.Language = code
	}

	// input history
	if v := session.Get("input"); !liveAbsent(v) {
		items, e := liveConvertInput(v, cfg.Instructions, cfg.Language, clientEventID)
		if e != nil {
			return nil, e
		}
		cfg.InitialItems = items
	}

	// delegation
	if !liveAbsent(delegation) {
		if t := delegation.Get("type"); !liveAbsent(t) {
			if t.Type != gjson.String {
				return fail("invalid_value", "session.delegation.type", "session.delegation.type must be a string.")
			}
			switch t.String() {
			case "client":
			case "responses":
				return fail("unsupported_value", "session.delegation.type", "Responses delegation is not supported; use delegation type client.")
			default:
				return fail("unsupported_value", "session.delegation.type", "Unsupported delegation type: '"+t.String()+"'. Use client.")
			}
		}
		if !liveAbsent(delegation.Get("responses")) {
			return fail("unsupported_value", "session.delegation.responses", "Responses delegation is not supported; use delegation type client.")
		}
		if d := delegation.Get("acknowledgement_delay_ms"); !liveAbsent(d) {
			ms, ok := liveWholeNumber(d)
			if !ok || ms < 0 {
				return fail("invalid_value", "session.delegation.acknowledgement_delay_ms", "session.delegation.acknowledgement_delay_ms must be an integer of 0 or more.")
			}
			cfg.AckDelayMS = ms
		}
		if a := delegation.Get("acknowledgement"); !liveAbsent(a) {
			if a.Type != gjson.String {
				return fail("invalid_value", "session.delegation.acknowledgement", "session.delegation.acknowledgement must be a string.")
			}
			switch a.String() {
			case "none":
				cfg.AckFiller = false
			case "auto":
				cfg.AckFiller = true
			default:
				return fail("unsupported_value", "session.delegation.acknowledgement", "Unsupported acknowledgement: '"+a.String()+"'. Use auto or none.")
			}
		}
	}

	// store
	if s := session.Get("store"); !liveAbsent(s) {
		if s.Type != gjson.True && s.Type != gjson.False {
			return fail("invalid_value", "session.store", "session.store must be a boolean.")
		}
		b := s.Bool()
		cfg.Store = &b
	}
	return cfg, nil
}

// liveConvertInput turns the public input history into internal initial_items.
// Internal roles are user, assistant and developer (Codex sends all three); the public system role maps to developer.
// The gateway prepends one developer message of its own for the instructions and one for the transcription language when they are set, so the input may hold that many items fewer than liveSessionMaxInputItems.
func liveConvertInput(input gjson.Result, instructions, language string, clientEventID string) ([]json.RawMessage, *liveSessionError) {
	fail := func(code, param, msg string) ([]json.RawMessage, *liveSessionError) {
		return nil, newLiveSessionError(code, param, msg, clientEventID)
	}
	if !input.IsArray() {
		return fail("invalid_value", "session.input", "session.input must be an array of messages.")
	}
	entries := input.Array()
	maxItems := liveSessionMaxInputItems
	var reserved []string
	if instructions != "" {
		maxItems--
		reserved = append(reserved, "session.instructions")
	}
	if language != "" {
		maxItems--
		reserved = append(reserved, "session.audio.input.transcription.language")
	}
	if len(entries) > maxItems {
		if len(reserved) == 1 && instructions != "" {
			return fail("invalid_value", "session.input", "session.input has more than 127 items when session.instructions is set, because the instructions are sent as one more item.")
		}
		if len(reserved) > 0 {
			return fail("invalid_value", "session.input", "session.input has more than "+liveItoa(maxItems)+" items when "+strings.Join(reserved, " and ")+" is set, because the gateway sends "+strings.Join(reserved, " and ")+" as "+liveItoa(len(reserved))+" more item(s).")
		}
		return fail("invalid_value", "session.input", "session.input has more than 128 items.")
	}
	var items []json.RawMessage
	totalBytes := 0
	for i, entry := range entries {
		param := "session.input." + liveItoa(i)
		if !entry.IsObject() {
			return fail("invalid_value", param, param+" must be an object.")
		}
		if t := entry.Get("type"); !liveAbsent(t) && t.String() != "message" {
			return fail("unsupported_value", param+".type", "Unsupported input item type: '"+t.String()+"'. Use message.")
		}
		role := entry.Get("role").String()
		partType, internalRole := "input_text", ""
		switch role {
		case "user":
			internalRole = "user"
		case "assistant":
			internalRole, partType = "assistant", "output_text"
		case "developer", "system":
			internalRole = "developer"
		case "":
			return fail("missing_required_parameter", param+".role", param+".role is required.")
		default:
			return fail("unsupported_value", param+".role", "Unsupported role: '"+role+"'. Use user, assistant, developer or system.")
		}
		content := entry.Get("content")
		var texts []string
		switch {
		case liveAbsent(content):
			return fail("missing_required_parameter", param+".content", param+".content is required.")
		case content.Type == gjson.String:
			texts = []string{content.String()}
		case content.IsArray():
			for j, part := range content.Array() {
				pp := param + ".content." + liveItoa(j)
				if !part.IsObject() {
					return fail("invalid_value", pp, pp+" must be an object.")
				}
				switch part.Get("type").String() {
				case "input_text", "output_text", "text":
				default:
					return fail("unsupported_value", pp+".type", "Unsupported content type: '"+part.Get("type").String()+"'. Use input_text, output_text or text.")
				}
				if part.Get("text").Type != gjson.String {
					return fail("invalid_value", pp+".text", pp+".text must be a string.")
				}
				texts = append(texts, part.Get("text").String())
			}
		default:
			return fail("invalid_value", param+".content", param+".content must be a string or an array of text parts.")
		}
		parts := make([]map[string]any, 0, len(texts))
		for _, text := range texts {
			totalBytes += len(text)
			parts = append(parts, map[string]any{"type": partType, "text": text})
		}
		if totalBytes/4 > liveSessionMaxInputTokens {
			return fail("invalid_value", "session.input", "session.input is longer than 8,192 tokens.")
		}
		raw, _ := json.Marshal(map[string]any{"type": "message", "role": internalRole, "content": parts})
		items = append(items, raw)
	}
	return items, nil
}

// parseLiveSessionStart validates the first client message: it must be {"type":"session.start","session":{...}} (optional event_id is the client_event_id); anything else is an error.
func parseLiveSessionStart(raw []byte) (*liveSessionConfig, *liveSessionError) {
	if !gjson.ValidBytes(raw) || !gjson.ParseBytes(raw).IsObject() {
		return nil, newLiveSessionError("invalid_value", "", "The first message must be a JSON object: session.start.", "")
	}
	root := gjson.ParseBytes(raw)
	clientEventID := liveEventID(root)
	if t := root.Get("type").String(); t != "session.start" {
		if t == "" {
			return nil, newLiveSessionError("missing_required_parameter", "type", "The first message must be session.start.", clientEventID)
		}
		return nil, newLiveSessionError("invalid_value", "type", "The first message must be session.start, got '"+t+"'.", clientEventID)
	}
	if e := liveUnknownKeys(root, "", []string{"type", "event_id", "session"}, clientEventID); e != nil {
		e.Param = strings.TrimPrefix(e.Param, ".")
		e.Message = "Unknown parameter: '" + e.Param + "'."
		return nil, e
	}
	session := root.Get("session")
	if liveAbsent(session) {
		return nil, newLiveSessionError("missing_required_parameter", "session", "session.start requires a session object.", clientEventID)
	}
	return parseLiveSessionObject(session, clientEventID)
}

func liveEventID(root gjson.Result) string {
	if v := root.Get("event_id"); v.Type == gjson.String {
		return v.String()
	}
	return ""
}

func liveItoa(n int) string {
	raw, _ := json.Marshal(n)
	return string(raw)
}

// UpstreamSession is the internal call-create session: {model:"gpt-live-1-codex", instructions, audio:{output:{voice}}, delegation:{type:"client", ack_filler:false only when disabled}, initial_items (when any)}.
// The transcription language, when set, adds one more developer message after the instructions one.
// Non-empty instructions are delivered twice: in the instructions field and as a developer message that leads initial_items, before the client's input, because ChatGPT Live does not apply the instructions field on its own (proven live on 2026-11-05).
// Empty instructions are left out of both: a call without them is proven to work and stays silent until the user speaks.
func (cfg *liveSessionConfig) UpstreamSession() json.RawMessage {
	delegation := map[string]any{"type": "client"}
	if !cfg.AckFiller {
		delegation["ack_filler"] = false
	}
	session := map[string]any{
		"model":      liveAudioDefaultModel,
		"audio":      map[string]any{"output": map[string]any{"voice": cfg.LiveVoice}},
		"delegation": delegation,
	}
	if cfg.Instructions != "" {
		session["instructions"] = cfg.Instructions
	}
	items := make([]any, 0, len(cfg.InitialItems)+1)
	if cfg.Instructions != "" {
		items = append(items, liveInstructionsItem(cfg.Instructions))
	}
	if cfg.Language != "" {
		items = append(items, transcriptionLanguageItem(cfg.Language))
	}
	for _, item := range cfg.InitialItems {
		items = append(items, item)
	}
	if len(items) > 0 {
		session["initial_items"] = items
	}
	raw, _ := json.Marshal(session)
	return raw
}

// PublicSession is the effective public session object for session.started (id, model, instructions, audio.format, audio.output.voice, delegation incl. extension acknowledgement, store).
func (cfg *liveSessionConfig) PublicSession(id string) map[string]any {
	ack := "auto"
	if !cfg.AckFiller {
		ack = "none"
	}
	session := map[string]any{
		"id":           id,
		"model":        cfg.Model,
		"instructions": cfg.Instructions,
		"audio": map[string]any{
			"format": map[string]any{"type": cfg.FormatType, "rate": cfg.FormatRate},
			"output": map[string]any{"voice": cfg.Voice},
		},
		"delegation": map[string]any{
			"type":                     "client",
			"acknowledgement_delay_ms": cfg.AckDelayMS,
			"acknowledgement":          ack,
		},
	}
	if cfg.Language != "" {
		session["audio"].(map[string]any)["input"] = map[string]any{"transcription": map[string]any{"language": cfg.Language}}
	}
	if cfg.Store != nil {
		session["store"] = *cfg.Store
	}
	return session
}

// liveAck is one outstanding append waiting for its internal acknowledgement.
type liveAck struct {
	event         string // public ack event type
	clientEventID string
	remaining     int // internal chunks still to be acknowledged
	seq           int // order in which the appends were made, across all queues
	failed        bool
	session       bool // the public ack is session.updated and carries the effective session
}

// liveTiming is the latest known start and end of a turn, in milliseconds.
type liveTiming struct {
	start, end int64
	ok         bool
}

// liveSessionTranslator keeps the state of one public GPT-Live session: the config, whether upstream has started, the sideband events queued before that, the FIFO acknowledgement queues, usage and the gateway-clock start.
// It is safe for concurrent use: Client, Upstream, Tick, Closed, SessionID, Started, expectAck and ackFor take its mutex, so a transport may run the client and upstream directions on different goroutines.
type liveSessionTranslator struct {
	mu        sync.Mutex
	cfg       *liveSessionConfig
	now       func() time.Time
	started   bool
	sessionID string
	queued    [][]byte
	acks      map[string][]*liveAck // keyed by delegation item id; session-level appends use ""
	startedAt time.Time
	usageAt   time.Time // when a usage update was last reported
	seconds   int       // last reported usage seconds, never decreases
	heard     [2]string // recent transcript text of the current turn: 0 user, 1 assistant
	timing    [2]liveTiming
	turnDir   map[string]int // turn id -> 0 user, 1 assistant, for the role-less turn.delta
	ackSeq    int
	barge     *audioBargeIn
	speech    audioSpeechTracker
	// fixScript converts input transcripts to the script of the transcription language (Gujarati); nil leaves them as they are.
	fixScript func(string) string

	// history is the conversation so far for a voice change: the session's input items, then every finished turn, within the item and token caps. histSeq numbers the items added at run time.
	history    []liveHistoryItem
	histBytes  int
	histSeq    int
	turnText   [2]string // transcript of the turn in progress, per direction (0 user, 1 assistant), for a reply cut by a voice change
	delegation []string  // delegation item ids that Live created and the client has not answered with a commentary append yet
	usageBase  int       // seconds used by the calls that a voice change replaced
	swapOK     bool      // the transport can replace the call, so a voice change is possible
}

// liveHistoryItem is one message of the conversation kept for a voice change.
type liveHistoryItem struct {
	seq   int // 0 for the items the session started with
	role  string
	text  string
	item  json.RawMessage
	bytes int
}

// liveVoiceChange is a validated session.update that needs a new call: the transport opens it, then asks the translator to switch.
type liveVoiceChange struct {
	Voice         string  // public voice name
	LiveVoice     string  // internal voice
	Instructions  *string // new effective instructions when the same update changes them
	ClientEventID string
}

func newLiveSessionTranslator(cfg *liveSessionConfig) *liveSessionTranslator {
	t := &liveSessionTranslator{cfg: cfg, now: time.Now, acks: map[string][]*liveAck{}, turnDir: map[string]int{}}
	t.barge = newAudioBargeIn(func() time.Time { return t.now() })
	if cfg != nil {
		t.fixScript = transcriptionScriptFix(cfg.Language)
		for _, item := range cfg.InitialItems {
			t.seedHistory(item)
		}
	}
	return t
}

// languageHint is the quiet append that tells Live the transcription language after the call started; nil when no language is set or it needs no hint. The caller holds the mutex.
func (t *liveSessionTranslator) languageHint() []byte {
	return transcriptionLanguageHintEventFor(t.cfg)
}

// AllowVoiceSwap tells the translator that its transport can replace the call, which makes a voice change in session.update possible.
func (t *liveSessionTranslator) AllowVoiceSwap() {
	t.mu.Lock()
	t.swapOK = true
	t.mu.Unlock()
}

// Speech turns the detector's changes on the client's audio into session.input_audio.speech_started {audio_start_ms} and session.input_audio.speech_stopped {audio_end_ms}; the times count milliseconds of audio the client has sent. The detector reports them well before ChatGPT Live does, so the upstream turn events add nothing for a turn the detector saw.
func (t *liveSessionTranslator) Speech(events []audioSpeechEvent) [][]byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out [][]byte
	for _, ev := range events {
		t.speech.Detector(ev)
		typ, key := "session.input_audio.speech_stopped", "audio_end_ms"
		if ev.Started {
			typ, key = "session.input_audio.speech_started", "audio_start_ms"
		}
		raw, _ := json.Marshal(map[string]any{"type": typ, key: ev.MS})
		out = append(out, raw)
	}
	return out
}

// liveClientResult is what one public client event asks the transport to do.
type liveClientResult struct {
	Upstream [][]byte         // internal sideband events to send now, in order (empty while queued before session.started)
	Client   [][]byte         // public events to write to the client now (errors, local acks)
	Audio    []byte           // decoded payload of session.input_audio.append (raw client-format bytes, base64 already decoded); the transport converts and writes it to the track
	Close    bool             // session.close: the transport closes with reason close_requested
	Swap     *liveVoiceChange // session.update with a new voice: the transport replaces the call
}

// Client translates one public client event. Sideband events are queued until upstream session.started, errors come back at once.
func (t *liveSessionTranslator) Client(raw []byte) liveClientResult {
	t.mu.Lock()
	defer t.mu.Unlock()
	var res liveClientResult
	fail := func(code, param, msg, clientEventID string) liveClientResult {
		res.Client = append(res.Client, newLiveSessionError(code, param, msg, clientEventID).Event())
		return res
	}
	if !gjson.ValidBytes(raw) || !gjson.ParseBytes(raw).IsObject() {
		return fail("invalid_value", "", "The event must be a JSON object.", "")
	}
	root := gjson.ParseBytes(raw)
	id := liveEventID(root)
	typ := root.Get("type")
	if typ.Type != gjson.String || typ.String() == "" {
		return fail("missing_required_parameter", "type", "The event needs a type.", id)
	}

	send := func(events ...[]byte) {
		if t.started {
			res.Upstream = append(res.Upstream, events...)
		} else {
			t.queued = append(t.queued, events...)
		}
	}
	// full reports whether n more events would overflow the queue held back before session.started.
	full := func(n int) bool { return !t.started && len(t.queued)+n > liveMaxQueuedEvents }
	tooMany := func() liveClientResult {
		return fail("invalid_value", "", "too many events before session.started", id)
	}
	appendEvent := func(ackEvent, channel string) liveClientResult {
		delegationID := ""
		if d := root.Get("delegation_id"); !liveAbsent(d) {
			if d.Type != gjson.String {
				return fail("invalid_value", "delegation_id", "delegation_id must be a string or null.", id)
			}
			delegationID = d.String()
		}
		events, e := liveContextAppendEvents(delegationID, channel, root.Get("content"))
		if e != nil {
			e.ClientEventID = id
			res.Client = append(res.Client, e.Event())
			return res
		}
		if full(len(events)) {
			return tooMany()
		}
		t.pushAck(delegationID, ackEvent, id, len(events))
		send(events...)
		return res
	}

	switch typ.String() {
	case "session.start":
		return fail("invalid_value", "type", "The session has already started.", id)

	case "session.input_audio.append":
		a := root.Get("audio")
		if liveAbsent(a) {
			return fail("missing_required_parameter", "audio", "audio is required.", id)
		}
		if a.Type != gjson.String {
			return fail("invalid_value", "audio", "audio must be a base64 string.", id)
		}
		pcm, err := base64.StdEncoding.DecodeString(a.String())
		if err != nil {
			var rawErr error
			if pcm, rawErr = base64.RawStdEncoding.DecodeString(a.String()); rawErr != nil {
				return fail("invalid_value", "audio", "audio is not valid base64.", id)
			}
		}
		res.Audio = pcm
		return res

	case "session.input_audio.mute":
		if full(1) {
			return tooMany()
		}
		send([]byte(`{"type":"input_audio.pause"}`))
		return res
	case "session.input_audio.unmute":
		if full(1) {
			return tooMany()
		}
		send([]byte(`{"type":"input_audio.resume"}`))
		return res

	case "session.commentary.append":
		return appendEvent("session.commentary.appended", "speakable")
	case "session.thinking.append":
		return appendEvent("session.thinking.appended", "commentary")
	case "session.instructions.append":
		// Instructions are context for later turns, never spoken: always the session-level quiet channel.
		events, e := liveContextAppendEvents("", "commentary", root.Get("content"))
		if e != nil {
			e.ClientEventID = id
			res.Client = append(res.Client, e.Event())
			return res
		}
		if full(len(events)) {
			return tooMany()
		}
		t.pushAck("", "session.instructions.appended", id, len(events))
		send(events...)
		return res

	case "session.update":
		return t.update(root, id)

	case "session.close":
		res.Close = true
		return res

	case "response.create", "response.item.create":
		return fail("unsupported_value", "type", "'"+typ.String()+"' is not supported: GPT-Live decides when to speak.", id)
	}
	return fail("invalid_value", "type", "Unknown event type: '"+typ.String()+"'.", id)
}

// liveUpdateFields is what a validated session.update asks for.
type liveUpdateFields struct {
	Instructions *string
	Voice        string // public voice name, "" when the update names none
	LiveVoice    string
}

// parseLiveUpdate validates a session.update object. It stops at the first field, in document order, that cannot be applied: an unknown field is unknown_parameter, a known field that is fixed after the start is immutable_field_update, and delegation.responses is unsupported_value. Only instructions and audio.output.voice can change. Nothing is applied here, so a rejected update changes nothing.
func parseLiveUpdate(session gjson.Result, clientEventID string) (liveUpdateFields, *liveSessionError) {
	var upd liveUpdateFields
	var failure *liveSessionError
	fail := func(code, param, msg string) bool {
		failure = newLiveSessionError(code, param, msg, clientEventID)
		return false
	}
	immutable := func(param string) bool {
		return fail("immutable_field_update", param, param+" cannot be updated after the session has started.")
	}
	session.ForEach(func(k, v gjson.Result) bool {
		switch name := k.String(); name {
		case "instructions":
			if liveAbsent(v) {
				return true
			}
			if v.Type != gjson.String {
				return fail("invalid_value", "session.instructions", "session.instructions must be a string.")
			}
			if len(v.String())/4 > liveSessionMaxInstructionTokens {
				return fail("invalid_value", "session.instructions", "session.instructions is longer than 16,384 tokens.")
			}
			text := v.String()
			upd.Instructions = &text
		case "audio":
			if liveAbsent(v) {
				return true
			}
			if !v.IsObject() {
				return fail("invalid_value", "session.audio", "session.audio must be an object.")
			}
			v.ForEach(func(ak, av gjson.Result) bool {
				switch aname := ak.String(); aname {
				case "output":
					if liveAbsent(av) {
						return true
					}
					if !av.IsObject() {
						return fail("invalid_value", "session.audio.output", "session.audio.output must be an object.")
					}
					if e := liveUnknownKeys(av, "session.audio.output", []string{"voice"}, clientEventID); e != nil {
						failure = e
						return false
					}
					voice := av.Get("voice")
					if liveAbsent(voice) {
						return true
					}
					if voice.Type != gjson.String {
						return fail("invalid_value", "session.audio.output.voice", "session.audio.output.voice must be a string.")
					}
					if strings.TrimSpace(voice.String()) == "" {
						return true
					}
					live, ok := liveSessionVoice(voice.String())
					if !ok {
						return fail("unsupported_value", "session.audio.output.voice", "Unsupported voice: '"+voice.String()+"'.")
					}
					upd.Voice, upd.LiveVoice = strings.ToLower(strings.TrimSpace(voice.String())), live
				case "format":
					return immutable("session.audio.format")
				case "input":
					if av.IsObject() {
						if e := liveUnknownKeys(av, "session.audio.input", []string{"transcription"}, clientEventID); e != nil {
							failure = e
							return false
						}
					}
					return immutable("session.audio.input")
				default:
					return fail("unknown_parameter", "session.audio."+aname, "Unknown parameter: 'session.audio."+aname+"'.")
				}
				return true
			})
			return failure == nil
		case "delegation":
			if v.IsObject() {
				responsesOnly := true
				v.ForEach(func(dk, _ gjson.Result) bool {
					if dk.String() != "responses" {
						responsesOnly = false
					}
					return responsesOnly
				})
				if responsesOnly {
					return fail("unsupported_value", "session.delegation.responses", "Responses delegation is not supported by this gateway.")
				}
			}
			return immutable("session.delegation")
		case "model", "input", "store":
			return immutable("session." + name)
		default:
			return fail("unknown_parameter", "session."+name, "Unknown parameter: 'session."+name+"'.")
		}
		return true
	})
	return upd, failure
}

// liveVoiceNeedsWebSocket is the message of a voice change on a surface that cannot replace the call.
const liveVoiceNeedsWebSocket = "session.audio.output.voice cannot be changed on this session: the media belongs to your own connection. Voice changes need the WebSocket surface (GET /v1/live/sessions), where the gateway can replace the call."

// update handles session.update; the caller holds the mutex. Instructions change on every surface: they go upstream as a quiet context append and session.updated follows its acknowledgement. A voice change needs a new call, so it is handed to the transport as res.Swap and is only possible when the transport called AllowVoiceSwap. Everything else is fixed after the start.
func (t *liveSessionTranslator) update(root gjson.Result, id string) liveClientResult {
	var res liveClientResult
	fail := func(e *liveSessionError) liveClientResult {
		res.Client = append(res.Client, e.Event())
		return res
	}
	session := root.Get("session")
	if !session.IsObject() {
		return fail(newLiveSessionError("missing_required_parameter", "session", "session.update requires a session object.", id))
	}
	if len(session.Map()) == 0 {
		return fail(newLiveSessionError("missing_required_parameter", "session", "session.update needs at least one field.", id))
	}
	upd, e := parseLiveUpdate(session, id)
	if e != nil {
		return fail(e)
	}
	needSwap := upd.Voice != "" && upd.LiveVoice != t.cfg.LiveVoice
	if needSwap && !t.swapOK {
		return fail(newLiveSessionError("immutable_field_update", "session.audio.output.voice", liveVoiceNeedsWebSocket, id))
	}
	if needSwap && !t.started {
		return fail(&liveSessionError{Code: "voice_change_failed", Param: "session.audio.output.voice", Message: "The voice cannot change before session.started. Send the change again once the session has started.", ClientEventID: id})
	}
	if needSwap {
		res.Swap = &liveVoiceChange{Voice: upd.Voice, LiveVoice: upd.LiveVoice, Instructions: upd.Instructions, ClientEventID: id}
		return res
	}
	if upd.Voice != "" {
		// Another public name for the voice the call already has.
		t.cfg.Voice = upd.Voice
	}
	if upd.Instructions != nil && *upd.Instructions != t.cfg.Instructions {
		events := liveQuietAppendEvents(liveInstructionsUpdateText(*upd.Instructions))
		if !t.started && len(t.queued)+len(events) > liveMaxQueuedEvents {
			return fail(newLiveSessionError("invalid_value", "", "too many events before session.started", id))
		}
		t.cfg.Instructions = *upd.Instructions
		t.pushAck("", "session.updated", id, len(events))
		t.acks[""][len(t.acks[""])-1].session = true
		if t.started {
			res.Upstream = append(res.Upstream, events...)
		} else {
			t.queued = append(t.queued, events...)
		}
		return res
	}
	res.Client = append(res.Client, t.sessionUpdatedEvent(id))
	return res
}

// liveInstructionsUpdateText is the text of the quiet append that carries new instructions to a running call.
func liveInstructionsUpdateText(instructions string) string {
	const lead = "Updated instructions for the rest of this call (these replace the earlier instructions): "
	if strings.TrimSpace(instructions) == "" {
		return lead + "there are no special instructions now."
	}
	return lead + instructions
}

// sessionUpdatedEvent renders session.updated with the effective session; the caller holds the mutex.
func (t *liveSessionTranslator) sessionUpdatedEvent(clientEventID string) []byte {
	var clientValue any
	if clientEventID != "" {
		clientValue = clientEventID
	}
	out, _ := json.Marshal(map[string]any{"type": "session.updated", "session": t.cfg.PublicSession(t.sessionID), "client_event_id": clientValue})
	return out
}

// liveQuietAppendEvents builds the session-level quiet context appends for a text: the commentary channel, which Live uses as context and does not speak. The text is chunked like every other append.
func liveQuietAppendEvents(text string) [][]byte {
	var events [][]byte
	for _, chunk := range liveChunkText(text, liveContextAppendMaxBytes) {
		raw, _ := json.Marshal(map[string]any{
			"type":    "session.context.append",
			"channel": "commentary",
			"content": []map[string]any{{"type": "input_text", "text": chunk}},
		})
		events = append(events, raw)
	}
	return events
}

// liveVoiceChangeFailedEvent is the extension error for a voice change that did not happen.
func liveVoiceChangeFailedEvent(message, clientEventID string) []byte {
	return liveErrorEvent("server_error", "voice_change_failed", message, "session.audio.output.voice", clientEventID)
}

// liveDelegationCancelledEvent tells the client that an open delegation did not survive a voice change. delegation_id on the error object is a gateway extension.
func liveDelegationCancelledEvent(id string) []byte {
	raw, _ := json.Marshal(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":            "invalid_request_error",
			"code":            "delegation_cancelled",
			"message":         "Delegation " + id + " was cancelled because the voice changed and the call was replaced. Live no longer knows this request: ask again or wait for the user to repeat it.",
			"param":           "delegation_id",
			"client_event_id": nil,
			"delegation_id":   id,
		},
	})
	return raw
}

// closeDelegation forgets an open delegation; the caller holds the mutex.
func (t *liveSessionTranslator) closeDelegation(id string) {
	for i, open := range t.delegation {
		if open == id {
			t.delegation = append(t.delegation[:i], t.delegation[i+1:]...)
			return
		}
	}
}

// seedHistory adds an input item of the session start to the conversation.
func (t *liveSessionTranslator) seedHistory(item json.RawMessage) {
	root := gjson.ParseBytes(item)
	var text strings.Builder
	for _, part := range root.Get("content").Array() {
		text.WriteString(part.Get("text").String())
	}
	t.pushHistory(liveHistoryItem{role: root.Get("role").String(), text: text.String(), item: item, bytes: text.Len()})
}

// addHistory adds a finished turn to the conversation. role is user or assistant.
func (t *liveSessionTranslator) addHistory(role, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	partType := "input_text"
	if role == "assistant" {
		partType = "output_text"
	}
	raw, _ := json.Marshal(map[string]any{"type": "message", "role": role, "content": []map[string]any{{"type": partType, "text": text}}})
	t.histSeq++
	t.pushHistory(liveHistoryItem{seq: t.histSeq, role: role, text: text, item: raw, bytes: len(text)})
}

// pushHistory keeps the conversation inside the caps: at most 128 items and 8,192 tokens, the oldest dropped first.
func (t *liveSessionTranslator) pushHistory(h liveHistoryItem) {
	t.history = append(t.history, h)
	t.histBytes += h.bytes
	for len(t.history) > 0 && (len(t.history) > liveSessionMaxInputItems || t.histBytes/4 > liveSessionMaxInputTokens) {
		t.histBytes -= t.history[0].bytes
		t.history = t.history[1:]
	}
}

// historyItems returns the newest part of the conversation that fits in the caps next to `reserved` developer items the gateway adds itself, oldest first.
func (t *liveSessionTranslator) historyItems(reserved int) []json.RawMessage {
	maxItems := liveSessionMaxInputItems - reserved
	bytes, start := 0, len(t.history)
	for i := len(t.history) - 1; i >= 0 && len(t.history)-i <= maxItems; i-- {
		if (bytes+t.history[i].bytes)/4 > liveSessionMaxInputTokens {
			break
		}
		bytes += t.history[i].bytes
		start = i
	}
	items := make([]json.RawMessage, 0, len(t.history)-start)
	for _, h := range t.history[start:] {
		items = append(items, h.item)
	}
	return items
}

// SwapConfig returns the session for the call that replaces this one: the effective session with the new voice, the effective instructions and the conversation so far as the input items. since marks how far the conversation had come, for Resume.
func (t *liveSessionTranslator) SwapConfig(change *liveVoiceChange) (cfg *liveSessionConfig, since int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	next := *t.cfg
	next.Voice, next.LiveVoice = change.Voice, change.LiveVoice
	if change.Instructions != nil {
		next.Instructions = *change.Instructions
	}
	reserved := 0
	if next.Instructions != "" {
		reserved++
	}
	if next.Language != "" {
		reserved++
	}
	next.InitialItems = t.historyItems(reserved)
	return &next, t.histSeq
}

// liveSwapOutcome is what switching to the new call asks the transport to write.
type liveSwapOutcome struct {
	Client   [][]byte // public events for the client, in order
	Upstream [][]byte // quiet appends for the new call
}

// Resume switches the translator to the new call, which has started. The reply in progress is cut, the open delegations and the appends that were never acknowledged are reported to the client as errors, whatever was said while the new call was opening is appended to the new call (since is the value SwapConfig returned), and usage keeps counting on top of the old call. The client then gets session.voice_changed and session.updated.
func (t *liveSessionTranslator) Resume(cfg *liveSessionConfig, since int, change *liveVoiceChange, gap time.Duration) liveSwapOutcome {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out liveSwapOutcome
	now := t.now()
	previous := t.cfg.Voice
	emit := func(event []byte) { out.Client = append(out.Client, event) }

	if t.barge.open {
		if text := t.turnText[1]; strings.TrimSpace(text) != "" {
			t.addHistory("assistant", text)
		}
		raw, _ := json.Marshal(liveInterruptedEvent(t.barge.openID))
		emit(raw)
	}
	for _, id := range t.delegation {
		emit(liveDelegationCancelledEvent(id))
	}
	var lost []*liveAck
	for _, queue := range t.acks {
		lost = append(lost, queue...)
	}
	sort.Slice(lost, func(i, j int) bool { return lost[i].seq < lost[j].seq })
	for _, ack := range lost {
		// A pending instructions update is carried by the new call, which has the instructions from the start.
		if ack.failed || ack.session || ack.clientEventID == "" {
			continue
		}
		emit(liveErrorEvent("server_error", "append_not_delivered", "The call was replaced by a voice change before this append was acknowledged, so Live may not have it. Send it again.", "", ack.clientEventID))
	}

	t.acks = map[string][]*liveAck{}
	if hint := transcriptionLanguageHintEventFor(cfg); hint != nil {
		// The new call gets the language hint right after it started, before the catch-up.
		out.Upstream = append(out.Upstream, hint)
		t.pushAck("", "", "", 1)
	}

	var missed strings.Builder
	for _, h := range t.history {
		if h.seq <= since {
			continue
		}
		role := "User"
		if h.role == "assistant" {
			role = "Assistant"
		}
		missed.WriteString("\n" + role + ": " + h.text)
	}
	if missed.Len() > 0 {
		events := liveQuietAppendEvents("The conversation continued while the voice was changing. Add this to what you remember:" + missed.String())
		t.pushAck("", "", "", len(events))
		out.Upstream = append(out.Upstream, events...)
	}

	t.usageBase = t.usage(now, 0)
	t.startedAt, t.usageAt = now, now
	t.cfg = cfg
	t.started = true
	t.queued = nil
	t.delegation = nil
	t.heard, t.timing, t.turnText = [2]string{}, [2]liveTiming{}, [2]string{}
	t.turnDir = map[string]int{}
	t.barge = newAudioBargeIn(func() time.Time { return t.now() })
	t.speech = audioSpeechTracker{}

	voiceChanged, _ := json.Marshal(map[string]any{"type": "session.voice_changed", "voice": change.Voice, "previous_voice": previous, "gap_ms": gap.Milliseconds()})
	emit(voiceChanged)
	emit(t.sessionUpdatedEvent(change.ClientEventID))
	return out
}

// liveContextAppendEvents builds the internal context append events for one public append.
// delegationID "" -> session.context.append; otherwise delegation.context.append for that item. channel is the internal channel ("speakable" is spoken, "commentary" is quiet).
// content is a string or an array of parts with text; the text is chunked at 500 bytes (UTF-8 safe), one event per chunk. Empty text yields one event with an empty text, which the internal side acknowledges and does not speak.
func liveContextAppendEvents(delegationID, channel string, content gjson.Result) ([][]byte, *liveSessionError) {
	var text string
	switch {
	case liveAbsent(content):
		return nil, newLiveSessionError("missing_required_parameter", "content", "content is required.", "")
	case content.Type == gjson.String:
		text = content.String()
	case content.IsArray():
		var b strings.Builder
		for i, part := range content.Array() {
			if part.Type == gjson.String {
				b.WriteString(part.String())
				continue
			}
			if !part.IsObject() || part.Get("text").Type != gjson.String {
				return nil, newLiveSessionError("invalid_value", "content."+liveItoa(i), "content parts need a text string.", "")
			}
			b.WriteString(part.Get("text").String())
		}
		text = b.String()
	default:
		return nil, newLiveSessionError("invalid_value", "content", "content must be a string or an array of text parts.", "")
	}
	var events [][]byte
	for _, chunk := range liveChunkText(text, liveContextAppendMaxBytes) {
		event := map[string]any{
			"type":    "session.context.append",
			"channel": channel,
			"content": []map[string]any{{"type": "input_text", "text": chunk}},
		}
		if delegationID != "" {
			event["type"] = "delegation.context.append"
			event["delegation_item_id"] = delegationID
		}
		raw, _ := json.Marshal(event)
		events = append(events, raw)
	}
	return events, nil
}

// liveChunkText splits text into pieces of at most max bytes without cutting a UTF-8 sequence. Empty text gives one empty chunk.
func liveChunkText(text string, max int) []string {
	if len(text) <= max {
		return []string{text}
	}
	var chunks []string
	for len(text) > 0 {
		end := max
		if end >= len(text) {
			chunks = append(chunks, text)
			break
		}
		for end > 0 && !utf8.RuneStart(text[end]) {
			end--
		}
		if end == 0 {
			end = max
		}
		chunks = append(chunks, text[:end])
		text = text[end:]
	}
	return chunks
}

// liveUpstreamResult is what one internal sideband event produces.
type liveUpstreamResult struct {
	Client   [][]byte // public events for the client, in order
	Upstream [][]byte // queued client events to flush now (only on session.started)
	AudioOut []byte   // decoded 24 kHz PCM16LE bytes of session.output_audio.delta; the transport converts and sends session.output_audio.delta itself (or drops it on WebRTC)
}

// Upstream translates one internal sideband event. Events with no public meaning produce an empty result.
//
// Transcripts: the sideband reports every word twice, as input_transcript.added / output_transcript.added (with item.text) and as a role-less turn.delta (same text and timings, only a turn_id). The added events carry the role in their type, so they are the single source and turn.delta is ignored; turn.created and turn.done carry no incremental text and are ignored too.
func (t *liveSessionTranslator) Upstream(raw []byte) liveUpstreamResult {
	t.mu.Lock()
	defer t.mu.Unlock()
	var res liveUpstreamResult
	if !gjson.ValidBytes(raw) || !gjson.ParseBytes(raw).IsObject() {
		return res
	}
	root := gjson.ParseBytes(raw)
	emit := func(payload map[string]any) {
		out, _ := json.Marshal(payload)
		res.Client = append(res.Client, out)
	}

	switch root.Get("type").String() {
	case "session.started":
		if t.started {
			return res
		}
		t.started = true
		t.sessionID = root.Get("session.id").String()
		t.startedAt = t.now()
		t.usageAt = t.startedAt
		emit(map[string]any{"type": "session.started", "session": t.cfg.PublicSession(t.sessionID)})
		res.Upstream, t.queued = t.queued, nil
		if hint := t.languageHint(); hint != nil {
			// The hint goes first, right after session.started; its acknowledgement is put first in the queue too, so the acks of the client's own appends keep their order.
			res.Upstream = append([][]byte{hint}, res.Upstream...)
			t.acks[""] = append([]*liveAck{{remaining: 1}}, t.acks[""]...)
		}

	case "session.output_audio.delta", "output_audio.delta":
		if pcm, err := base64.StdEncoding.DecodeString(root.Get("delta").String()); err == nil {
			res.AudioOut = pcm
		}

	case "input_transcript.added", "output_transcript.added":
		delta := liveFirstString(root, "item.text", "delta", "text")
		if delta == "" {
			return res
		}
		dir, typ := 0, "session.input_transcript.delta"
		if root.Get("type").String() == "output_transcript.added" {
			dir, typ = 1, "session.output_transcript.delta"
		} else if t.fixScript != nil {
			delta = t.fixScript(delta)
		}
		event := map[string]any{"type": typ, "delta": delta, "start_ms": root.Get("start_ms").Int(), "end_ms": root.Get("end_ms").Int()}
		// An added event without timings takes the latest ones reported for the same role by turn.created or turn.delta.
		if latest := t.timing[dir]; latest.ok {
			if !root.Get("start_ms").Exists() {
				event["start_ms"] = latest.start
			}
			if !root.Get("end_ms").Exists() {
				event["end_ms"] = latest.end
			}
		}
		// Deltas are word fragments, too short to identify a language, so the detector reads the accumulated text of the current turn (the last 200 bytes of it, for this direction only).
		// The language is put on every delta from the moment the detector is reliable; the accumulator is reset when the turn of that role ends.
		t.heard[dir] = liveTrimTail(t.heard[dir]+delta, liveLanguageWindow)
		if dir == 1 && len(t.turnText[1]) < liveTurnTextMax {
			t.turnText[1] += delta
		}
		if dir == 0 && t.cfg != nil && t.cfg.Language != "" {
			// The client named the language, so it is reported as given instead of the detector's guess.
			event["language"] = t.cfg.Language
			if script := transcriptScript(t.cfg.Language, t.heard[dir]); script != "" {
				event["script"] = script
			}
		} else if lang := liveTranscriptLanguage(t.heard[dir]); lang != "" {
			event["language"] = lang
		}
		emit(event)

	case "turn.created":
		role := realtimeAudioTurnRole(root)
		if dir, ok := liveTurnDir(role); ok {
			if id := root.Get("turn.id").String(); id != "" {
				t.turnDir[id] = dir
			}
			t.noteTiming(dir, root.Get("turn.start_ms"), root.Get("turn.end_ms"))
		}
		if role == "user" && t.speech.TurnCreated() {
			// No detector saw this speech (a WebRTC attach, or too quiet): report the start from the upstream turn, on the session clock.
			emit(map[string]any{"type": "session.input_audio.speech_started", "audio_start_ms": root.Get("turn.start_ms").Int(), "source": "upstream"})
		}
		if turnID, interrupted := t.barge.Turn("created", role, root.Get("turn.id").String()); interrupted {
			emit(liveInterruptedEvent(turnID))
		}

	case "turn.delta":
		// Text comes from the added events (see above); turn.delta only refreshes the timings of its turn's role.
		if dir, ok := t.turnDir[root.Get("turn_id").String()]; ok {
			t.noteTiming(dir, root.Get("start_ms"), root.Get("end_ms"))
		}

	case "turn.done":
		role := realtimeAudioTurnRole(root)
		if dir, ok := liveTurnDir(role); ok {
			t.heard[dir], t.timing[dir], t.turnText[dir] = "", liveTiming{}, ""
			delete(t.turnDir, root.Get("turn.id").String())
			if transcript := root.Get("turn.transcript"); transcript.Type == gjson.String {
				text := transcript.String()
				if dir == 0 && t.fixScript != nil {
					text = t.fixScript(text)
				}
				t.addHistory(role, text)
			}
		}
		if role == "user" && t.speech.TurnDone() {
			emit(map[string]any{"type": "session.input_audio.speech_stopped", "audio_end_ms": root.Get("turn.end_ms").Int(), "source": "upstream"})
		}
		if turnID, interrupted := t.barge.Turn("done", role, root.Get("turn.id").String()); interrupted {
			emit(liveInterruptedEvent(turnID))
		}

	case "delegation.created":
		if out, ok := liveDelegationCreatedEvent(raw); ok {
			res.Client = append(res.Client, out)
			id := root.Get("item.id").String()
			open := false
			for _, known := range t.delegation {
				open = open || known == id
			}
			if !open {
				t.delegation = append(t.delegation, id)
			}
		}

	case "delegation.context.appended":
		if out := t.ack(root.Get("delegation_item_id").String()); out != nil {
			res.Client = append(res.Client, out)
		}
	case "session.context.appended":
		if out := t.ack(""); out != nil {
			res.Client = append(res.Client, out)
		}

	case "input_audio.paused":
		emit(map[string]any{"type": "session.input_audio.muted"})
	case "input_audio.resumed":
		emit(map[string]any{"type": "session.input_audio.unmuted"})

	case "session.usage.updated":
		upstreamSeconds := int(math.Ceil(root.Get("usage.audio_duration_ms").Float() / 1000))
		emit(map[string]any{"type": "session.usage.updated", "usage": map[string]any{"seconds": t.usage(t.now(), upstreamSeconds)}})

	case "error":
		e := root.Get("error")
		message := liveFirstString(e, "message")
		if message == "" {
			message = "Upstream error."
		}
		code, param := liveFirstString(e, "code"), liveFirstString(e, "param")
		// Internal errors do not name the event they answer. An error that is about a context append (param content* or delegation_item_id*, or an unknown delegation) means that append will never be acknowledged, so the oldest pending acknowledgement is failed to keep the FIFO aligned and its client_event_id goes on the error. Any other error leaves the acks alone.
		clientEventID := ""
		if strings.HasPrefix(param, "content") || strings.HasPrefix(param, "delegation_item_id") || strings.Contains(code, "delegation") || strings.Contains(strings.ToLower(message), "unknown delegation") {
			clientEventID = t.failOldestAck()
		}
		res.Client = append(res.Client, liveErrorEvent(liveFirstString(e, "type"), code, message, param, clientEventID))
	}
	return res
}

// transcriptionLanguageHintEventFor is the language hint for a session config, nil when it has no language.
func transcriptionLanguageHintEventFor(cfg *liveSessionConfig) []byte {
	if cfg == nil || cfg.Language == "" {
		return nil
	}
	return transcriptionLanguageHintEvent(cfg.Language)
}

// liveInterruptedEvent is the gateway extension session.output_audio.interrupted: the user spoke over the assistant, so the assistant reply was cut. The client should drop the reply audio it has queued. turn_id is the upstream id of the cut assistant turn.
func liveInterruptedEvent(turnID string) map[string]any {
	event := map[string]any{"type": "session.output_audio.interrupted"}
	if turnID != "" {
		event["turn_id"] = turnID
	}
	return event
}

func liveFirstString(root gjson.Result, paths ...string) string {
	for _, path := range paths {
		if v := root.Get(path); v.Type == gjson.String && v.String() != "" {
			return v.String()
		}
	}
	return ""
}

// liveTurnDir maps a turn role to its direction index: 0 user, 1 assistant.
func liveTurnDir(role string) (int, bool) {
	switch role {
	case "user":
		return 0, true
	case "assistant":
		return 1, true
	}
	return 0, false
}

func (t *liveSessionTranslator) noteTiming(dir int, start, end gjson.Result) {
	if !start.Exists() && !end.Exists() {
		return
	}
	t.timing[dir] = liveTiming{start: start.Int(), end: end.Int(), ok: true}
}

// pushAck records an outstanding append; the caller holds the mutex.
func (t *liveSessionTranslator) pushAck(key, event, clientEventID string, chunks int) {
	t.ackSeq++
	t.acks[key] = append(t.acks[key], &liveAck{event: event, clientEventID: clientEventID, remaining: chunks, seq: t.ackSeq})
}

// expectAck records an outstanding append made outside the translator (the Realtime bridge sends its own appends), taking the mutex. An empty event produces no public event when acknowledged.
func (t *liveSessionTranslator) expectAck(key, event, clientEventID string, chunks int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pushAck(key, event, clientEventID, chunks)
}

// ackFor pops the oldest outstanding append for key, taking the mutex; see ack.
func (t *liveSessionTranslator) ackFor(key string) []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ack(key)
}

// Started reports whether upstream session.started has arrived.
func (t *liveSessionTranslator) Started() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.started
}

// failOldestAck marks the oldest pending append across all queues as failed and returns its client event id. The entry stays queued until its remaining chunks are resolved, so later acks still match later appends, and it never produces an ack event.
func (t *liveSessionTranslator) failOldestAck() string {
	var oldest *liveAck
	var key string
	for k, queue := range t.acks {
		if len(queue) > 0 && (oldest == nil || queue[0].seq < oldest.seq) {
			oldest, key = queue[0], k
		}
	}
	if oldest == nil {
		return ""
	}
	clientEventID := oldest.clientEventID
	oldest.failed = true
	// Every error of a chunked append resolves one chunk; a failed entry whose chunks are all resolved is dropped.
	if oldest.remaining--; oldest.remaining <= 0 {
		t.popAck(key)
	}
	return clientEventID
}

func (t *liveSessionTranslator) popAck(key string) {
	if queue := t.acks[key]; len(queue) <= 1 {
		delete(t.acks, key)
	} else {
		t.acks[key] = queue[1:]
	}
}

// ack pops the oldest outstanding append for key and renders its public ack once its last chunk is acknowledged; the caller holds the mutex. A failed append resolves silently.
func (t *liveSessionTranslator) ack(key string) []byte {
	queue := t.acks[key]
	if len(queue) == 0 {
		return nil
	}
	head := queue[0]
	head.remaining--
	if head.remaining > 0 {
		return nil
	}
	t.popAck(key)
	if head.failed || head.event == "" {
		return nil
	}
	if head.session {
		return t.sessionUpdatedEvent(head.clientEventID)
	}
	var clientValue any
	if head.clientEventID != "" {
		clientValue = head.clientEventID
	}
	out, _ := json.Marshal(map[string]any{"type": head.event, "client_event_id": clientValue})
	return out
}

// usage returns the reported usage seconds at now: the larger of the upstream figure, the whole seconds since session.started on the gateway clock and the last reported value.
func (t *liveSessionTranslator) usage(now time.Time, upstreamSeconds int) int {
	seconds := upstreamSeconds
	if t.started {
		if elapsed := int(now.Sub(t.startedAt) / time.Second); elapsed > seconds {
			seconds = elapsed
		}
	}
	// The calls that a voice change replaced were used too.
	seconds += t.usageBase
	if seconds > t.seconds {
		t.seconds = seconds
	}
	t.usageAt = now
	return t.seconds
}

// Tick is called by the transport about once a second; it returns a gateway-clock session.usage.updated when upstream has sent none for 60 s.
func (t *liveSessionTranslator) Tick(now time.Time) [][]byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.started || now.Sub(t.usageAt) < liveUsageSilence {
		return nil
	}
	out, _ := json.Marshal(map[string]any{"type": "session.usage.updated", "usage": map[string]any{"seconds": t.usage(now, 0)}})
	return [][]byte{out}
}

// Closed renders session.closed {reason, usage:{seconds}}; reason is close_requested, expired, remote_hangup or connection_lost.
func (t *liveSessionTranslator) Closed(reason string) []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	seconds := t.seconds
	if t.started {
		seconds = t.usage(t.now(), 0)
	}
	out, _ := json.Marshal(map[string]any{"type": "session.closed", "reason": reason, "usage": map[string]any{"seconds": seconds}})
	return out
}

// SessionID returns the id from upstream session.started ("" before).
func (t *liveSessionTranslator) SessionID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessionID
}

// liveDelegationCreatedEvent converts an internal delegation.created into the public session.delegation.created, with the task text in the documented gateway extension delegation.content (the input_text parts joined).
func liveDelegationCreatedEvent(raw []byte) ([]byte, bool) {
	root := gjson.ParseBytes(raw)
	if root.Get("type").String() != "delegation.created" {
		return nil, false
	}
	id := root.Get("item.id").String()
	if id == "" {
		return nil, false
	}
	var content strings.Builder
	if c := root.Get("item.content"); c.Type == gjson.String {
		content.WriteString(c.String())
	} else {
		for _, part := range c.Array() {
			if part.Get("type").String() == "input_text" {
				content.WriteString(part.Get("text").String())
			}
		}
	}
	out, _ := json.Marshal(map[string]any{
		"type":      "session.delegation.created",
		"offset_ms": root.Get("offset_ms").Int(),
		"delegation": map[string]any{
			"id":      id,
			"type":    "delegation",
			"target":  "client",
			"content": content.String(),
		},
	})
	return out, true
}

// liveTrimTail keeps at most max bytes from the end of text, without cutting a UTF-8 sequence.
func liveTrimTail(text string, max int) string {
	if len(text) <= max {
		return text
	}
	cut := len(text) - max
	for cut < len(text) && !utf8.RuneStart(text[cut]) {
		cut++
	}
	return text[cut:]
}
