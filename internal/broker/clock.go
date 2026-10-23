package broker

import (
	"sync"
	"time"
)

// Clock is the single source of "now" for every time-driven decision (token
// expiry, refresh skew, profile-cache TTL). Injecting it keeps those decisions
// pure functions of Now() + stored state, so tests advance a StoppedClock
// instead of waiting on real time.
type Clock interface {
	Now() time.Time
}

type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now() }

type StoppedClock struct {
	mu sync.Mutex
	t  time.Time
}

func NewStoppedClock(t time.Time) *StoppedClock { return &StoppedClock{t: t} }

func (c *StoppedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *StoppedClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}
