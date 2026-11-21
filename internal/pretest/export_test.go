package pretest

import "time"

// SetTimings shortens the protocol's waits for tests, and returns a function
// that puts them back.
func SetTimings(interval, timeout, up time.Duration) func() {
	i, t, u := pingInterval, pingTimeout, upDuration
	pingInterval, pingTimeout, upDuration = interval, timeout, up
	return func() { pingInterval, pingTimeout, upDuration = i, t, u }
}
