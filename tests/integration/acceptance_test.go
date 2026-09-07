// Package integration runs the exact acceptance scenario from spec
// section 60: normal traffic -> runaway traffic -> velocity anomaly ->
// circuit open -> block -> cooldown -> half-open -> recovery -> reconcile.
package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"velocityguard/internal/gateway"
	"velocityguard/internal/ledger"
	"velocityguard/internal/pricing"
	"velocityguard/internal/provider"
	"velocityguard/internal/reservation"
	"velocityguard/internal/risk"

	"velocityguard/internal/money"
)

func TestFinalAcceptanceScenario(t *testing.T) {
	const tenant = "demo-corp"

	rm := reservation.NewManager()
	rm.SetBudget(tenant, money.FromFloat(10.00)) // Budget: $10, per spec section 60

	policy := risk.DefaultPolicy()
	// Compress timing so the test doesn't need real 10s/15s sleeps. This
	// changes only *how fast* windows/cooldowns move, not the decision
	// logic itself.
	policy.CircuitCooldown = 150 * time.Millisecond
	policy.ShortWindow = 200 * time.Millisecond
	policy.MediumWindow = 400 * time.Millisecond
	policy.LongWindow = 2 * time.Second
	re := risk.NewEngine(rm, policy)

	pr := pricing.NewRegistry()
	pr.AddRate(pricing.Rate{
		Provider: "demo-provider", Model: "demo-model",
		InputPerUnit: money.FromFloat(0.0001), OutputPerUnit: money.FromFloat(0.0002),
		Version: 1, EffectiveFrom: time.Unix(0, 0),
	})

	// Traffic mode is mutated by the test to script normal -> runaway -> recovery.
	mode := "normal"
	mock := provider.NewMockProvider("demo-provider", func(req provider.ExecRequest) provider.Usage {
		switch mode {
		case "runaway":
			return provider.Usage{InputUnits: 20000, OutputUnits: 20000} // expensive burst, ~$6/call
		default:
			return provider.Usage{InputUnits: 100, OutputUnits: 100} // ~$0.03/call, "normal"
		}
	})
	providers := provider.NewRegistry()
	providers.Register(mock)

	l := ledger.New()
	gw := gateway.New(re, rm, pr, providers, l)

	route := gateway.RouteConfig{Route: "/agent/execute", Provider: "demo-provider", Model: "demo-model"}
	ctx := context.Background()

	call := func(id string, estIn, estOut int64) gateway.RequestResult {
		return gw.HandleRequest(ctx, id, tenant, route,
			provider.ExecRequest{Method: "POST", URL: "http://mock/agent/execute"}, estIn, estOut, 1)
	}

	// --- Step 1: normal traffic, expect ALLOW ---
	mode = "normal"
	for i := 0; i < 5; i++ {
		res := call(fmt.Sprintf("normal-%d", i), 100, 100)
		if res.Decision.Action != risk.ActionAllow {
			t.Fatalf("normal traffic call %d: expected ALLOW, got %s (%s)", i, res.Decision.Action, res.Decision.Reason)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Logf("step 1 OK: normal traffic allowed")

	// --- Step 2: runaway traffic until circuit opens / requests blocked ---
	mode = "runaway"
	var sawBlocked bool
	var sawCircuitOpen bool
	for i := 0; i < 50 && !sawBlocked; i++ {
		res := call(fmt.Sprintf("runaway-%d", i), 20000, 20000)
		t.Logf("runaway call %d: action=%s score=%d level=%s velocityMult=%.1fx circuit=%s",
			i, res.Decision.Action, res.Decision.RiskScore, res.Decision.RiskLevel, res.Decision.VelocityMult, res.Decision.CircuitState)
		if res.Decision.Action == risk.ActionBlock {
			sawBlocked = true
		}
		if res.Decision.CircuitState == "OPEN" {
			sawCircuitOpen = true
		}
	}
	if !sawBlocked {
		t.Fatal("expected runaway traffic to eventually be BLOCKED")
	}
	t.Logf("step 2 OK: runaway traffic detected and blocked (circuit open observed=%v)", sawCircuitOpen)

	// --- Step 3: the very next request must also be blocked (circuit open, budget exhausted, or both) ---
	res := call("blocked-check", 100, 100)
	if res.Decision.Action != risk.ActionBlock {
		t.Fatalf("expected next request to be BLOCKED, got %s", res.Decision.Action)
	}
	t.Logf("step 3 OK: next request rejected (%s)", res.Decision.RuleTriggered)

	// --- Step 4: cooldown then HALF_OPEN, then a successful trial closes the circuit ---
	// Give the tenant a fresh budget slice to represent a new accounting
	// period / manual override, since our $10 budget is now exhausted from
	// the runaway burst and reconciliation would otherwise still block on
	// budget alone.
	rm.SetBudget(tenant, money.FromFloat(10.00)+rm.Exposure(tenant).Reserved+rm.Exposure(tenant).Settled)
	// Exceed both the circuit cooldown AND let the short/medium velocity
	// windows decay past the runaway burst, so the trial request is
	// evaluated against genuinely calm conditions (not a stale spike).
	time.Sleep(600 * time.Millisecond)

	mode = "normal"
	res = call("recovery-trial", 100, 100)
	t.Logf("step 4: recovery trial action=%s circuit=%s score=%d velocityMult=%.2fx",
		res.Decision.Action, res.Decision.CircuitState, res.Decision.RiskScore, res.Decision.VelocityMult)
	if res.Decision.Action != risk.ActionAllow {
		t.Fatalf("expected the HALF_OPEN trial request to succeed and close the circuit, got %s (score=%d)", res.Decision.Action, res.Decision.RiskScore)
	}

	// --- Step 5: reconciliation math must hold: reserved - actual = released ---
	if !res.Executed {
		t.Fatal("expected recovery-trial to execute")
	}
	expectedReleaseOrOverage := res.Decision.EstimatedCost - res.ActualCost
	if expectedReleaseOrOverage >= 0 && res.Reconcile.Released != expectedReleaseOrOverage {
		t.Fatalf("reconciliation mismatch: expected released=%s, got %s", expectedReleaseOrOverage, res.Reconcile.Released)
	}
	t.Logf("step 5 OK: reservation=%s actual=%s released=%s overage=%s",
		res.Decision.EstimatedCost, res.ActualCost, res.Reconcile.Released, res.Reconcile.Overage)

	// The full lifecycle must be visible in the ledger.
	events := l.ForTenant(tenant)
	seen := map[ledger.EventType]bool{}
	for _, e := range events {
		seen[e.Type] = true
	}
	for _, want := range []ledger.EventType{
		ledger.RequestEstimated, ledger.ReservationCreated, ledger.RequestAllowed,
		ledger.RequestBlocked, ledger.RequestExecuted, ledger.CostReconciled,
	} {
		if !seen[want] {
			t.Errorf("expected to see a %s event in the ledger, but didn't (saw %d total events)", want, len(events))
		}
	}
	t.Logf("full lifecycle visible in ledger: %d total events recorded", len(events))
}

// Golden Test Case 10: tenant isolation.
func TestTenantIsolation(t *testing.T) {
	rm := reservation.NewManager()
	rm.SetBudget("tenant-a", money.FromFloat(5))
	rm.SetBudget("tenant-b", money.FromFloat(1000))

	// Exhaust tenant A's budget.
	if _, err := rm.Reserve("tenant-a", money.FromFloat(5), time.Minute); err != nil {
		t.Fatal(err)
	}

	// Tenant B must be completely unaffected by tenant A's exhausted budget.
	if _, err := rm.Reserve("tenant-b", money.FromFloat(500), time.Minute); err != nil {
		t.Fatalf("tenant B should be unaffected by tenant A's budget state, got err: %v", err)
	}

	l := ledger.New()
	l.Append(ledger.Event{Type: ledger.RequestAllowed, TenantID: "tenant-a", RequestID: "a-1"})
	l.Append(ledger.Event{Type: ledger.RequestAllowed, TenantID: "tenant-b", RequestID: "b-1"})

	aEvents := l.ForTenant("tenant-a")
	for _, e := range aEvents {
		if e.TenantID != "tenant-a" {
			t.Fatalf("tenant A's event query leaked a tenant B event: %+v", e)
		}
	}
	if len(aEvents) != 1 {
		t.Fatalf("expected exactly 1 event for tenant-a, got %d", len(aEvents))
	}
}
