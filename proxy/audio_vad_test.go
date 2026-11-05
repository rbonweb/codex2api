package proxy

import (
	"encoding/binary"
	"math"
	"testing"
	"time"
)

// vadSamples is the number of 24 kHz samples in ms milliseconds.
func vadSamples(ms int) int { return audioVADSampleRate / 1000 * ms }

func vadPCM(samples []int16) []byte {
	out := make([]byte, 2*len(samples))
	for i, s := range samples {
		binary.LittleEndian.PutUint16(out[2*i:], uint16(s))
	}
	return out
}

func vadSilence(ms int) []byte { return vadPCM(make([]int16, vadSamples(ms))) }

// vadTone is a sine of the given peak amplitude; its RMS is amplitude/sqrt(2).
func vadTone(ms int, amplitude, hz float64) []byte {
	samples := make([]int16, vadSamples(ms))
	for i := range samples {
		samples[i] = int16(amplitude * math.Sin(2*math.Pi*hz*float64(i)/audioVADSampleRate))
	}
	return vadPCM(samples)
}

// vadNoise is deterministic white noise with the given peak amplitude.
func vadNoise(ms int, amplitude float64, seed uint32) []byte {
	samples := make([]int16, vadSamples(ms))
	state := seed
	for i := range samples {
		state = state*1664525 + 1013904223
		samples[i] = int16((float64(state>>16)/32768 - 1) * amplitude)
	}
	return vadPCM(samples)
}

// vadSpeechLike is syllable bursts: a voiced tone with an amplitude envelope, one burst every period ms with gap ms of quiet between them.
func vadSpeechLike(bursts, burstMS, gapMS int, amplitude float64) []byte {
	var out []byte
	for i := 0; i < bursts; i++ {
		samples := make([]int16, vadSamples(burstMS))
		for j := range samples {
			envelope := math.Sin(math.Pi * float64(j) / float64(len(samples)))
			carrier := math.Sin(2*math.Pi*180*float64(j)/audioVADSampleRate) + 0.5*math.Sin(2*math.Pi*540*float64(j)/audioVADSampleRate)
			samples[j] = int16(amplitude * (0.2 + 0.8*envelope) * carrier / 1.5)
		}
		out = append(out, vadPCM(samples)...)
		out = append(out, vadSilence(gapMS)...)
	}
	return out
}

func vadConcat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func vadRun(v *audioVAD, pcm []byte) []audioSpeechEvent { return v.Process(pcm) }

func TestVADSilenceAndLowNoiseNeverTrigger(t *testing.T) {
	for name, pcm := range map[string][]byte{
		"digital silence": vadSilence(3000),
		"low noise":       vadNoise(3000, 120, 1), // RMS about 70, far under the 300 floor
		"room noise":      vadNoise(3000, 400, 2), // RMS about 230, still under the floor
	} {
		if events := vadRun(newAudioVAD(), pcm); len(events) != 0 {
			t.Errorf("%s produced %v", name, events)
		}
	}
}

func TestVADToneStartsAndStopsWithTimes(t *testing.T) {
	v := newAudioVAD()
	pcm := vadConcat(vadSilence(500), vadTone(1000, 8000, 220), vadSilence(800))
	events := vadRun(v, pcm)
	if len(events) != 2 || !events[0].Started || events[1].Started {
		t.Fatalf("events = %v", events)
	}
	if events[0].MS != 500 {
		t.Errorf("audio_start_ms = %d, want 500", events[0].MS)
	}
	if events[1].MS != 1500 {
		t.Errorf("audio_end_ms = %d, want 1500 (the end of the last speech frame)", events[1].MS)
	}
	if v.Speaking() {
		t.Error("still speaking after the stop")
	}
	if v.PositionMS() != 2300 {
		t.Errorf("position = %d ms, want 2300", v.PositionMS())
	}
}

func TestVADStartNeedsAbout100msAndStopAbout500ms(t *testing.T) {
	// 80 ms of tone is not speech.
	if events := vadRun(newAudioVAD(), vadConcat(vadSilence(200), vadTone(80, 8000, 220), vadSilence(1000))); len(events) != 0 {
		t.Fatalf("a short click produced %v", events)
	}
	// 100 ms of tone is.
	events := vadRun(newAudioVAD(), vadConcat(vadTone(100, 8000, 220), vadSilence(1000)))
	if len(events) != 2 {
		t.Fatalf("100 ms tone produced %v", events)
	}
	// 400 ms of silence keeps the segment open, 500 ms ends it.
	v := newAudioVAD()
	if events := vadRun(v, vadConcat(vadTone(300, 8000, 220), vadSilence(400))); len(events) != 1 || !events[0].Started || !v.Speaking() {
		t.Fatalf("400 ms of silence ended the segment: %v", events)
	}
	if events := vadRun(v, vadSilence(100)); len(events) != 1 || events[0].Started || events[0].MS != 300 {
		t.Fatalf("500 ms of silence did not end it at 300 ms: %v", events)
	}
}

func TestVADChunkingDoesNotChangeTheResult(t *testing.T) {
	pcm := vadConcat(vadSilence(300), vadSpeechLike(4, 220, 120, 9000), vadSilence(900))
	want := vadRun(newAudioVAD(), pcm)
	if len(want) != 2 {
		t.Fatalf("whole = %v", want)
	}
	for _, chunk := range []int{1, 7, 333, 960, 4801} {
		v := newAudioVAD()
		var got []audioSpeechEvent
		for i := 0; i < len(pcm); i += chunk {
			end := i + chunk
			if end > len(pcm) {
				end = len(pcm)
			}
			got = append(got, v.Process(pcm[i:end])...)
		}
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Errorf("chunk %d: %v, want %v", chunk, got, want)
		}
	}
}

func TestVADSpeechLikeBurstsFormOneSegmentUnderTheStopTime(t *testing.T) {
	// Syllables with 150 ms gaps are one utterance.
	events := vadRun(newAudioVAD(), vadConcat(vadSilence(200), vadSpeechLike(5, 200, 150, 7000), vadSilence(700)))
	if len(events) != 2 || !events[0].Started || events[1].Started {
		t.Fatalf("events = %v", events)
	}
	// Gaps of 700 ms split them.
	events = vadRun(newAudioVAD(), vadConcat(vadSpeechLike(2, 300, 700, 7000), vadSilence(100)))
	if len(events) != 4 {
		t.Fatalf("two utterances produced %v", events)
	}
	for i, started := range []bool{true, false, true, false} {
		if events[i].Started != started {
			t.Errorf("event %d = %v", i, events[i])
		}
	}
}

func TestVADThresholdAdaptsToSteadyNoise(t *testing.T) {
	// Noise at RMS about 260 is under the floor, so it never triggers, and it lifts the threshold to about 3 times its level.
	v := newAudioVAD()
	noise := vadNoise(4000, 450, 3)
	if events := vadRun(v, noise); len(events) != 0 {
		t.Fatalf("noise produced %v", events)
	}
	if v.threshold() <= audioVADMinRMS {
		t.Errorf("threshold = %.0f, want it above the minimum %.0f", v.threshold(), audioVADMinRMS)
	}
	// A tone at the noise level plus a bit stays under the raised threshold; a clear voice crosses it.
	if events := vadRun(v, vadTone(500, 700, 220)); len(events) != 0 {
		t.Errorf("a weak tone crossed the raised threshold: %v", events)
	}
	if events := vadRun(v, vadTone(300, 9000, 220)); len(events) != 1 || !events[0].Started {
		t.Errorf("a clear voice did not start speech: %v", events)
	}
}

func TestVADResetEndsAnOpenSegment(t *testing.T) {
	v := newAudioVAD()
	vadRun(v, vadConcat(vadSilence(100), vadTone(400, 8000, 220)))
	if !v.Speaking() {
		t.Fatal("not speaking")
	}
	ev, ok := v.Reset()
	if !ok || ev.Started || ev.MS != 500 {
		t.Fatalf("reset = %v %v", ev, ok)
	}
	if _, ok := v.Reset(); ok {
		t.Error("second reset reported a stop")
	}
}

func TestVADBargeInTracker(t *testing.T) {
	now := time.Unix(1000, 0)
	b := newAudioBargeIn(func() time.Time { return now })

	// A user turn while an assistant turn is open interrupts it, once.
	b.Turn("created", "assistant", "a1")
	if id, ok := b.Turn("created", "user", "u1"); !ok || id != "a1" {
		t.Fatalf("user over open assistant = %q %v", id, ok)
	}
	if _, ok := b.Turn("done", "assistant", "a1"); ok {
		t.Error("the cut turn.done fired again")
	}
	if _, ok := b.Turn("created", "user", "u2"); ok {
		t.Error("a user turn with no assistant turn fired")
	}

	// An assistant turn.done and a user turn.created in the same moment is the same barge-in.
	b.Turn("created", "assistant", "a2")
	b.Turn("done", "assistant", "a2")
	now = now.Add(10 * time.Millisecond)
	if id, ok := b.Turn("created", "user", "u3"); !ok || id != "a2" {
		t.Fatalf("done then user = %q %v", id, ok)
	}

	// A user who speaks after the reply ended is not a barge-in.
	b.Turn("created", "assistant", "a3")
	b.Turn("done", "assistant", "a3")
	now = now.Add(2 * time.Second)
	if id, ok := b.Turn("created", "user", "u4"); ok {
		t.Fatalf("late user turn interrupted %q", id)
	}
}

func TestVADSpeechTrackerOwnership(t *testing.T) {
	var s audioSpeechTracker
	// Nothing from the detector: the upstream events report the turn.
	if !s.TurnCreated() || !s.TurnDone() {
		t.Fatal("upstream did not report a turn the detector missed")
	}
	// The detector saw the speech: the upstream events add nothing.
	s.Detector(audioSpeechEvent{Started: true, MS: 100})
	s.Detector(audioSpeechEvent{Started: false, MS: 900})
	if s.TurnCreated() || s.TurnDone() {
		t.Fatal("upstream reported a turn the detector saw")
	}
	// The next turn is not owned by the earlier segment.
	if !s.TurnCreated() || !s.TurnDone() {
		t.Fatal("the segment of an earlier turn owned the next one")
	}
	// A segment that starts after turn.created belongs to the next turn.
	s.TurnCreated()
	s.Detector(audioSpeechEvent{Started: true, MS: 5000})
	s.TurnDone()
	if s.TurnCreated() {
		t.Fatal("the next turn's segment was reported twice")
	}
}
