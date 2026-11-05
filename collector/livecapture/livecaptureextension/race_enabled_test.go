//go:build race

package livecaptureextension

// raceEnabled reports a -race build, whose instrumentation allocates where
// a normal build does not.
const raceEnabled = true
