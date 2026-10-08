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
	start   time.Duration
	max     time.Duration
}

func newBackoff(maxSeconds int) *backoff {
	return &backoff{current: time.Second, start: time.Second, max: time.Duration(maxSeconds) * time.Second}
}

// newBackoffBetween is newBackoff with an explicit starting delay, for callers whose first retry should not come
// after one second.
func newBackoffBetween(start, max time.Duration) *backoff {
	return &backoff{current: start, start: start, max: max}
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
	b.current = b.start
}
