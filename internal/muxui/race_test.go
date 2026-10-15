//go:build race

package muxui

// The race detector maps terabytes of shadow memory, which no address-space
// limit a test sets leaves room for.
func init() { raceDetector = true }
