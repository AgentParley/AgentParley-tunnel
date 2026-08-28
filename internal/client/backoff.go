package client

import (
	"math/rand"
	"time"
)

// backoff produces a jittered exponential reconnect delay, doubling from 1s up to
// SshEgressLimits.MaxReconnectBackoffSeconds (60s), forever — the design doc's "jittered exponential backoff, 1 s
// → 60 s, forever" for a dropped network connection.
type backoff struct {
	current time.Duration
	max     time.Duration
}

func newBackoff(maxSeconds int) *backoff {
	return &backoff{current: time.Second, max: time.Duration(maxSeconds) * time.Second}
}

func (b *backoff) next() time.Duration {
	wait := b.current
	b.current *= 2
	if b.current > b.max {
		b.current = b.max
	}

	jitter := time.Duration(rand.Int63n(int64(wait) / 2))
	return wait/2 + jitter
}

func (b *backoff) reset() {
	b.current = time.Second
}
