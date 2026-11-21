package pretest

// Limits are a test ticket's caps, as the platform gives them (the Miner's
// leasetoken.TestLimits). The client never asks for more.
type Limits struct {
	Pings        int   `json:"pings"`
	MaxDownBytes int64 `json:"max_down_bytes"`
	MaxUpBytes   int64 `json:"max_up_bytes"`
	MaxSeconds   int   `json:"max_seconds"`
}

// The Miner's hard maxima, whatever a token says.
const (
	MinerMaxPings     = 50
	MinerMaxDownBytes = 10_000_000
	MinerMaxUpBytes   = 10_000_000
	MinerMaxSeconds   = 10
)

// Clamp returns the limits a Miner applies: the token's, but never above its
// hard maxima.
func (l Limits) Clamp() Limits {
	return Limits{
		Pings:        min(max(l.Pings, 0), MinerMaxPings),
		MaxDownBytes: min(max(l.MaxDownBytes, 0), MinerMaxDownBytes),
		MaxUpBytes:   min(max(l.MaxUpBytes, 0), MinerMaxUpBytes),
		MaxSeconds:   min(max(l.MaxSeconds, 0), MinerMaxSeconds),
	}
}
