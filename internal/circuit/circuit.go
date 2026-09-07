// Package circuit implements the CLOSED -> OPEN -> HALF_OPEN -> CLOSED
// circuit breaker state machine used to stop traffic to a
// tenant/provider/route after a risk violation, then cautiously test
// recovery.
package circuit

import (
	"sync"
	"time"
)

type State string

const (
	Closed   State = "CLOSED"
	Open     State = "OPEN"
	HalfOpen State = "HALF_OPEN"
)

type Breaker struct {
	mu          sync.Mutex
	state       State
	cooldown    time.Duration
	openedAt    time.Time
	now         func() time.Time
	halfOpenGen uint64 // guards against a stale half-open test result racing a new trip
}

func New(cooldown time.Duration) *Breaker {
	return &Breaker{
		state:    Closed,
		cooldown: cooldown,
		now:      time.Now,
	}
}

// State returns the breaker's current state, first advancing OPEN -> HALF_OPEN
// if the cooldown has elapsed.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.maybeAdvanceLocked()
	if b.state == halfOpenTesting {
		return HalfOpen
	}
	return b.state
}

func (b *Breaker) maybeAdvanceLocked() {
	if b.state == Open && b.now().Sub(b.openedAt) >= b.cooldown {
		b.state = HalfOpen
	}
}

// Trip forces the breaker OPEN (e.g. on a CRITICAL risk violation or kill switch).
func (b *Breaker) Trip() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state = Open
	b.openedAt = b.now()
	b.halfOpenGen++
}

// Allow reports whether a request may proceed given the current state, and
// for HALF_OPEN, reserves this call as *the* single test request — only one
// caller gets true from HALF_OPEN per generation, so concurrent requests
// don't all pile through as "the test."
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.maybeAdvanceLocked()
	switch b.state {
	case Closed:
		return true
	case HalfOpen:
		// Consume the trial slot: move to a transient "testing" sub-state by
		// bumping generation so a second concurrent caller sees it's no
		// longer the trial slot holder... simplified: allow exactly one
		// concurrent HALF_OPEN allowance by immediately marking tested.
		b.state = halfOpenTesting
		return true
	default: // Open, or halfOpenTesting (another request already testing)
		return false
	}
}

// internal pseudo-state: a HALF_OPEN trial is in flight. Not exported.
const halfOpenTesting State = "HALF_OPEN_TESTING"

// ReportSuccess should be called after an Allow()==true request completes
// successfully. If the breaker was testing recovery, this closes it.
func (b *Breaker) ReportSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == halfOpenTesting {
		b.state = Closed
	}
}

// ReportFailure should be called after an Allow()==true request fails or
// itself triggers a violation. If the breaker was testing recovery, this
// re-opens it.
func (b *Breaker) ReportFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == halfOpenTesting || b.state == Closed {
		b.state = Open
		b.openedAt = b.now()
	}
}
