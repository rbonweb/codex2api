//go:build race

package promptfilter

// raceDetectorEnabled marks that this test build has -race enabled. This package's production code has no goroutines,
// pure-CPU large-sample scanning tests run more than 20 times slower under race detection and find no races at all,
// so these tests only run in normal builds (see skipHeavyFixtureUnderRace).
const raceDetectorEnabled = true
