package pretest

import "time"

// SetTimings shortens the protocol's waits for tests, and returns a function
// that puts them back.
func SetTimings(interval, timeout, window, up time.Duration) func() {
	i, t, w, u := pingInterval, pingTimeout, pingWindow, upDuration
	pingInterval, pingTimeout, pingWindow, upDuration = interval, timeout, window, up
	return func() { pingInterval, pingTimeout, pingWindow, upDuration = i, t, w, u }
}
