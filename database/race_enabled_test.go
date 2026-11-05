//go:build race

package database

// raceDetectorEnabled marks that this test build has -race enabled. Race detection inflates CPU/memory
// overhead by 5-20x, so performance-bound assertions need their time limit relaxed accordingly, while normal builds stay strict.
const raceDetectorEnabled = true
