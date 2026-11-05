package proxy

import (
	"math"
	"time"
)

// This file holds the gateway-side voice activity detection and the barge-in tracker shared by /v1/live/sessions and /v1/realtime.
// ChatGPT Live reports no speech event: the first word reaches the sideband about 1.5 s after the user started speaking and the end of the user turn about 2 s after the last word, so the gateway watches the client's own audio where it can.

const (
	// audioVADSampleRate is the rate of the PCM the detector reads: the 24 kHz PCM16 the gateway feeds to ChatGPT Live.
	audioVADSampleRate = 24000
	// audioVADFrameMS is the length of one analysed frame.
	audioVADFrameMS = 20
	// audioVADFrameSamples is the number of samples in one frame.
	audioVADFrameSamples = audioVADSampleRate / 1000 * audioVADFrameMS
	// audioVADStartMS is how much speech, as consecutive frames above the threshold, starts a speech segment.
	audioVADStartMS = 100
	// audioVADStopMS is how much silence, as consecutive frames below the threshold, ends a speech segment.
	audioVADStopMS = 500
	// audioVADMinRMS is the lowest threshold, as a 16-bit sample RMS: about -41 dBFS, above room noise and below a quiet voice.
	audioVADMinRMS = 300.0
	// audioVADNoiseRatio is how far above the tracked noise floor a frame must be to count as speech.
	audioVADNoiseRatio = 3.0
	// audioVADHangover is the share of the threshold a frame must stay above while a segment runs, so a dip between two words does not count as silence.
	audioVADHangover = 0.6
	// audioVADFloorRate is how fast the noise floor follows frames that are not speech (a slow moving average).
	audioVADFloorRate = 0.05
	// audioVADMaxFloor caps the noise floor so a loud constant sound cannot make speech undetectable for good.
	audioVADMaxFloor = 3000.0
)

// audioSpeechEvent is one change reported by the detector. MS is audio_start_ms for a start and audio_end_ms for a stop, counted in milliseconds of audio the detector has received.
type audioSpeechEvent struct {
	Started bool
	MS      int64
}

// audioVAD is an energy voice activity detector with an adaptive threshold over 24 kHz PCM16 little-endian audio.
// A segment starts once audioVADStartMS of speech frames in a row cross the threshold (max of audioVADMinRMS and audioVADNoiseRatio times the noise floor) and ends after audioVADStopMS of quiet frames in a row.
// The noise floor is a slow moving average of the frames outside a segment, so steady background noise raises the threshold instead of triggering it.
// It is not safe for concurrent use.
type audioVAD struct {
	carry    []byte
	frames   int64 // whole frames analysed so far
	floor    float64
	speaking bool
	run      int // consecutive frames of the pending state change
	// startCandidate is where the run of speech frames that may start a segment began; lastSpeechEnd is the end of the last speech frame of the open segment. Both are on the audio clock.
	startCandidate int64
	lastSpeechEnd  int64
}

func newAudioVAD() *audioVAD { return &audioVAD{} }

// Speaking reports whether a speech segment is open.
func (v *audioVAD) Speaking() bool { return v.speaking }

// PositionMS is the length of audio analysed so far.
func (v *audioVAD) PositionMS() int64 { return v.frames * audioVADFrameMS }

// threshold is the RMS a frame needs to count as speech.
func (v *audioVAD) threshold() float64 {
	return math.Max(audioVADMinRMS, v.floor*audioVADNoiseRatio)
}

// Process analyses PCM16LE audio of any chunking and returns the speech changes it completes, in order.
func (v *audioVAD) Process(pcm []byte) []audioSpeechEvent {
	const frameBytes = audioVADFrameSamples * 2
	data := pcm
	if len(v.carry) > 0 {
		data = append(append([]byte(nil), v.carry...), pcm...)
		v.carry = nil
	}
	var events []audioSpeechEvent
	for len(data) >= frameBytes {
		if ev, ok := v.frame(audioFrameRMS(data[:frameBytes])); ok {
			events = append(events, ev)
		}
		data = data[frameBytes:]
	}
	if len(data) > 0 {
		v.carry = append([]byte(nil), data...)
	}
	return events
}

// frame advances the state machine by one frame with the given RMS.
func (v *audioVAD) frame(rms float64) (audioSpeechEvent, bool) {
	start := v.frames * audioVADFrameMS
	v.frames++
	end := v.frames * audioVADFrameMS
	threshold := v.threshold()
	if v.speaking {
		if rms >= threshold*audioVADHangover {
			v.run, v.lastSpeechEnd = 0, end
			return audioSpeechEvent{}, false
		}
		v.run++
		if v.run*audioVADFrameMS >= audioVADStopMS {
			v.speaking, v.run = false, 0
			return audioSpeechEvent{Started: false, MS: v.lastSpeechEnd}, true
		}
		return audioSpeechEvent{}, false
	}
	if rms >= threshold {
		if v.run == 0 {
			v.startCandidate = start
		}
		v.run++
		if v.run*audioVADFrameMS >= audioVADStartMS {
			v.speaking, v.run, v.lastSpeechEnd = true, 0, end
			return audioSpeechEvent{Started: true, MS: v.startCandidate}, true
		}
		return audioSpeechEvent{}, false
	}
	v.run = 0
	v.floor += (math.Min(rms, audioVADMaxFloor) - v.floor) * audioVADFloorRate
	return audioSpeechEvent{}, false
}

// Reset drops the carried bytes and the open segment and returns the stop event when a segment was open. The audio clock keeps running, and the noise floor is kept.
func (v *audioVAD) Reset() (audioSpeechEvent, bool) {
	v.carry, v.run = nil, 0
	if !v.speaking {
		return audioSpeechEvent{}, false
	}
	v.speaking = false
	return audioSpeechEvent{Started: false, MS: v.lastSpeechEnd}, true
}

// audioFrameRMS is the RMS of one frame of PCM16LE bytes.
func audioFrameRMS(frame []byte) float64 {
	n := len(frame) / 2
	if n == 0 {
		return 0
	}
	var sum float64
	for i := 0; i < n; i++ {
		s := float64(int16(uint16(frame[2*i]) | uint16(frame[2*i+1])<<8))
		sum += s * s
	}
	return math.Sqrt(sum / float64(n))
}

// audioBargeInWindow is how close in time an assistant turn.done and a user turn.created must arrive to count as one barge-in. ChatGPT Live reports them in the same millisecond; the window leaves room for the two events to be read one after the other.
const audioBargeInWindow = 250 * time.Millisecond

// audioBargeIn recognises the barge-in ChatGPT Live shows on the sideband: a user turn.created while an assistant turn is open, or an assistant turn.done (a reply cut mid-sentence) with a user turn.created in the same moment. There is no explicit interrupt event upstream.
// It is not safe for concurrent use.
type audioBargeIn struct {
	now         func() time.Time
	openID      string // assistant turn open now
	open        bool
	doneID      string // last assistant turn that ended
	doneAt      time.Time
	interrupted string // last assistant turn reported as interrupted
}

func newAudioBargeIn(now func() time.Time) *audioBargeIn {
	if now == nil {
		now = time.Now
	}
	return &audioBargeIn{now: now}
}

// Turn feeds one turn event: kind is "created" or "done", role the turn role ("user" or "assistant") and id the turn id. It returns the interrupted assistant turn id and true when this event shows a barge-in, at most once per assistant turn.
func (b *audioBargeIn) Turn(kind, role, id string) (string, bool) {
	switch {
	case role == "assistant" && kind == "created":
		b.open, b.openID = true, id
	case role == "assistant" && kind == "done":
		if id == "" {
			id = b.openID
		}
		b.open, b.openID = false, ""
		b.doneID, b.doneAt = id, b.now()
	case role == "user" && kind == "created":
		if b.open && b.openID != b.interrupted {
			b.interrupted = b.openID
			return b.openID, true
		}
		if !b.open && b.doneID != "" && b.doneID != b.interrupted && b.now().Sub(b.doneAt) <= audioBargeInWindow {
			b.interrupted = b.doneID
			return b.doneID, true
		}
	}
	return "", false
}

// audioSpeechTracker decides who reports the speech of a user turn: the detector when it saw the audio, the upstream turn events when it did not (a WebRTC attach, or speech too quiet for the threshold).
// A user turn is "owned" by the detector when a speech segment started since the previous user turn.created; the upstream events then add nothing, so a client never sees a start or stop twice.
// It is not safe for concurrent use.
type audioSpeechTracker struct {
	segments        int  // detector segments started since the last user turn.created
	turnOwned       bool // the current user turn was reported by the detector
	upstreamStarted bool // the gateway reported the current user turn's start from the upstream event
}

// Detector records a change reported by the detector.
func (s *audioSpeechTracker) Detector(ev audioSpeechEvent) {
	if ev.Started {
		s.segments++
	}
}

// TurnCreated is called on an upstream user turn.created; it reports whether the start must be reported from the upstream event.
func (s *audioSpeechTracker) TurnCreated() bool {
	s.turnOwned = s.segments > 0
	s.segments = 0
	s.upstreamStarted = !s.turnOwned
	return s.upstreamStarted
}

// TurnDone is called on an upstream user turn.done; it reports whether the stop must be reported from the upstream event.
func (s *audioSpeechTracker) TurnDone() bool {
	report := s.upstreamStarted
	s.turnOwned, s.upstreamStarted = false, false
	return report
}
