package promptfilter

import "testing"

// skipHeavyFixtureUnderRace is for single-threaded, pure-CPU tests of huge encoding/compression samples: they run in
// -race, each takes 3-45 seconds with no concurrency for the race detector to check. A normal build (the CI test job)
// still runs in full, and semantic coverage is unchanged.
func skipHeavyFixtureUnderRace(t *testing.T) {
	t.Helper()
	if raceDetectorEnabled {
		t.Skip("CPU-bound single-threaded fixture; covered by the non-race test job")
	}
}
