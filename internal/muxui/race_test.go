//go:build race

package muxui

// The race detector's shadow memory is private writable memory, which a data
// limit counts: the test that runs under one of 2 GiB needs between 1280 and
// 1536 MiB under -race, against 448 without, measured at rest and not under
// load, so it still skips there.
func init() { raceDetector = true }
