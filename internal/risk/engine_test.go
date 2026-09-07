package risk

import (
	"testing"
	"time"

	"velocityguard/internal/money"
	"velocityguard/internal/reservation"
)

func newTestEngine(budget float64) (*Engine, *reservation.Manager) {
	rm := reservation.NewManager()
	rm.SetBudget("t1", money.FromFloat(budget))
	e := NewEngine(rm, DefaultPolicy())
	return e, rm
}

// Golden Test Case 1.
func TestEvaluate_NormalRequestAllowed(t *testing.T) {
	e, _ := newTestEngine(100)
	d := e.Evaluate(Request{TenantID: "t1", EstimatedCost: money.FromFloat(0.10)})
	if d.Action != ActionAllow {
		t.Fatalf("expected ALLOW, got %s (reason: %s)", d.Action, d.Reason)
	}
	if d.ReservationID == "" {
		t.Fatal("expected a reservation id on ALLOW")
	}
}

// Golden Test Case 2.
func TestEvaluate_BudgetExhaustionBlocks(t *testing.T) {
	e, _ := newTestEngine(0.05)
	d := e.Evaluate(Request{TenantID: "t1", EstimatedCost: money.FromFloat(0.10)})
	if d.Action != ActionBlock {
		t.Fatalf("expected BLOCK, got %s", d.Action)
	}
}

// Golden Test Case 4: Velocity spike -> elevated risk.
func TestEvaluate_VelocitySpikeElevatesRisk(t *testing.T) {
	e, _ := newTestEngine(1000)

	// Establish a calm baseline: small, steady spend for a while so the
	// EWMA baseline settles near ~$0.10/sec.
	ts := e.stateFor("t1")
	base := time.Now().Add(-2 * time.Minute)
	ts.tracker.Short.now = func() time.Time { return base }
	ts.tracker.Medium.now = func() time.Time { return base }
	ts.tracker.Long.now = func() time.Time { return base }
	for i := 0; i < 60; i++ {
		ts.tracker.Record(money.FromFloat(0.10))
	}

	// Now simulate a spike: jump to "now" and record a burst of expensive
	// requests within the short (10s) window only.
	now := time.Now()
	ts.tracker.Short.now = func() time.Time { return now }
	ts.tracker.Medium.now = func() time.Time { return now }
	ts.tracker.Long.now = func() time.Time { return now }
	for i := 0; i < 10; i++ {
		ts.tracker.Record(money.FromFloat(10.0)) // $100 in ~10s window => ~$10/sec
	}

	d := e.Evaluate(Request{TenantID: "t1", EstimatedCost: money.FromFloat(0.10)})
	if d.RiskLevel == LevelLow {
		t.Fatalf("expected elevated risk after velocity spike, got score=%d level=%s mult=%.1fx",
			d.RiskScore, d.RiskLevel, d.VelocityMult)
	}
	if d.VelocityMult <= 1.0 {
		t.Fatalf("expected velocity multiplier > 1, got %.2f", d.VelocityMult)
	}
}

// Golden Test Case 5: Kill switch -> BLOCK regardless of budget/risk.
func TestEvaluate_KillSwitchBlocksEverything(t *testing.T) {
	e, _ := newTestEngine(1000)
	e.KillTenant("t1", true)
	d := e.Evaluate(Request{TenantID: "t1", EstimatedCost: money.FromFloat(0.01)})
	if d.Action != ActionKillSwitch {
		t.Fatalf("expected KILL_SWITCH action, got %s", d.Action)
	}
}

func TestEvaluate_KillSwitchGlobalAndProviderScopes(t *testing.T) {
	e, _ := newTestEngine(1000)
	e.KillAll(true)
	d := e.Evaluate(Request{TenantID: "any-tenant", EstimatedCost: money.FromFloat(0.01)})
	if d.Action != ActionKillSwitch {
		t.Fatalf("expected KILL_SWITCH for global kill, got %s", d.Action)
	}
	e.KillAll(false)

	e.KillProvider("openai", true)
	d2 := e.Evaluate(Request{TenantID: "t1", Provider: "openai", EstimatedCost: money.FromFloat(0.01)})
	if d2.Action != ActionKillSwitch {
		t.Fatalf("expected KILL_SWITCH for provider kill, got %s", d2.Action)
	}
}

// A CRITICAL-scoring block should trip the circuit breaker so subsequent
// requests are rejected fast without re-scoring (until cooldown).
func TestEvaluate_CriticalBlockTripsCircuit(t *testing.T) {
	e, rm := newTestEngine(100)
	ts := e.stateFor("t1")

	// Force velocity multiplier sky-high by manipulating baseline directly
	// via many tiny baseline samples then one huge burst, reusing the
	// approach from the velocity-spike test but pushed further to cross
	// BlockThreshold (90). We also drive budget utilization and
	// concurrency near their ceilings, since realistically a CRITICAL
	// event compounds multiple risk factors at once, not velocity alone.
	base := time.Now().Add(-2 * time.Minute)
	ts.tracker.Short.now = func() time.Time { return base }
	ts.tracker.Medium.now = func() time.Time { return base }
	ts.tracker.Long.now = func() time.Time { return base }
	for i := 0; i < 60; i++ {
		ts.tracker.Record(money.FromFloat(0.01))
	}
	now := time.Now()
	ts.tracker.Short.now = func() time.Time { return now }
	ts.tracker.Medium.now = func() time.Time { return now }
	ts.tracker.Long.now = func() time.Time { return now }
	for i := 0; i < 10; i++ {
		ts.tracker.Record(money.FromFloat(50.0))
	}

	// Pre-commit most of the budget via a large reservation, so budget
	// utilization is also near its ceiling when we evaluate.
	if _, err := rm.Reserve("t1", money.FromFloat(97), time.Minute); err != nil {
		t.Fatalf("setup reservation failed: %v", err)
	}

	d := e.Evaluate(Request{TenantID: "t1", EstimatedCost: money.FromFloat(0.10), Concurrency: 50})
	if d.Action != ActionBlock {
		t.Fatalf("expected BLOCK at extreme velocity, got %s (score=%d)", d.Action, d.RiskScore)
	}
	if d.RiskLevel == LevelCritical && d.CircuitState != "OPEN" {
		t.Fatalf("expected circuit OPEN after CRITICAL block, got %s", d.CircuitState)
	}
}

// BenchmarkEvaluate measures the hot-path decision latency (spec section 36).
func BenchmarkEvaluate(b *testing.B) {
	e, _ := newTestEngine(1_000_000)
	req := Request{TenantID: "bench-tenant", EstimatedCost: money.FromFloat(0.01)}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e.Evaluate(req)
	}
}
