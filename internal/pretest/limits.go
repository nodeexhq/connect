package pretest

// Limits are a test ticket's caps, as the platform gives them (the Node app's
// leasetoken.TestLimits). The client never asks for more.
type Limits struct {
	Pings        int   `json:"pings"`
	MaxDownBytes int64 `json:"max_down_bytes"`
	MaxUpBytes   int64 `json:"max_up_bytes"`
	MaxSeconds   int   `json:"max_seconds"`
}

// The Node app's hard maxima, whatever a token says.
const (
	NodeMaxPings     = 50
	NodeMaxDownBytes = 10_000_000
	NodeMaxUpBytes   = 10_000_000
	NodeMaxSeconds   = 10
)

// Clamp returns the limits the Node app applies: the token's, but never above its
// hard maxima.
func (l Limits) Clamp() Limits {
	return Limits{
		Pings:        min(max(l.Pings, 0), NodeMaxPings),
		MaxDownBytes: min(max(l.MaxDownBytes, 0), NodeMaxDownBytes),
		MaxUpBytes:   min(max(l.MaxUpBytes, 0), NodeMaxUpBytes),
		MaxSeconds:   min(max(l.MaxSeconds, 0), NodeMaxSeconds),
	}
}
