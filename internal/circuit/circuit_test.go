package circuit

import (
	"testing"
	"time"
)

// Golden Test Case 9: OPEN -> cooldown -> HALF_OPEN -> successful request -> CLOSED.
func TestCircuitRecoveryCycle(t *testing.T) {
	b := New(5 * time.Second)
	fixed := time.Now()
	b.now = func() time.Time { return fixed }

	if b.State() != Closed {
		t.Fatalf("expected initial CLOSED, got %s", b.State())
	}

	b.Trip()
	if b.State() != Open {
		t.Fatalf("expected OPEN after trip, got %s", b.State())
	}
	if b.Allow() {
		t.Fatal("expected Allow()==false while OPEN")
	}

	// Not enough time has passed yet.
	b.now = func() time.Time { return fixed.Add(2 * time.Second) }
	if b.State() != Open {
		t.Fatalf("expected still OPEN before cooldown elapses, got %s", b.State())
	}

	// Cooldown elapses.
	b.now = func() time.Time { return fixed.Add(6 * time.Second) }
	if b.State() != HalfOpen {
		t.Fatalf("expected HALF_OPEN after cooldown, got %s", b.State())
	}

	if !b.Allow() {
		t.Fatal("expected the single trial request to be allowed in HALF_OPEN")
	}
	b.ReportSuccess()
	if b.State() != Closed {
		t.Fatalf("expected CLOSED after successful trial, got %s", b.State())
	}
}

// Failed HALF_OPEN test must return to OPEN, not CLOSED.
func TestCircuitFailedTrialReturnsToOpen(t *testing.T) {
	b := New(1 * time.Second)
	fixed := time.Now()
	b.now = func() time.Time { return fixed }
	b.Trip()
	b.now = func() time.Time { return fixed.Add(2 * time.Second) }

	if !b.Allow() {
		t.Fatal("expected trial allowed")
	}
	b.ReportFailure()
	if b.State() != Open {
		t.Fatalf("expected OPEN after failed trial, got %s", b.State())
	}
}

// Only one concurrent request should get the HALF_OPEN trial slot.
func TestCircuitHalfOpenSingleTrial(t *testing.T) {
	b := New(1 * time.Second)
	fixed := time.Now()
	b.now = func() time.Time { return fixed }
	b.Trip()
	b.now = func() time.Time { return fixed.Add(2 * time.Second) }

	first := b.Allow()
	second := b.Allow()
	if !first {
		t.Fatal("expected first Allow() to get the trial slot")
	}
	if second {
		t.Fatal("expected second concurrent Allow() to be rejected while a trial is in flight")
	}
}
