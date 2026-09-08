// This file proves the core financial guarantee — reserved + settled
// never exceeds a tenant's budget — holds through the actual HTTP layer
// under real concurrent load, not just at the reservation-package unit
// level. Unlike internal/reservation's TestConcurrentOverspend_*, which
// calls Manager.Reserve directly, this drives 100+ concurrent goroutines
// through net/http against a real httptest.Server, exercising auth,
// scope checks, the gateway pipeline, and reconciliation together.
package integration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"velocityguard/internal/gateway"
	"velocityguard/internal/httpapi"
	"velocityguard/internal/ledger"
	"velocityguard/internal/money"
	"velocityguard/internal/pricing"
	"velocityguard/internal/provider"
	"velocityguard/internal/reservation"
	"velocityguard/internal/risk"
	"velocityguard/internal/store"
)

func TestHTTPConcurrency_NeverExceedsBudget(t *testing.T) {
	const (
		numRequests = 150  // > 100, per the P0 requirement
		budget      = 1.00 // $1.00 total
		costPerCall = 0.03 // ~$0.03/call at 100/100 units and the rates below
	)

	ctx := context.Background()
	st := store.NewMemoryStore()
	tenant, err := st.CreateTenant(ctx, "Concurrency Co", "concurrency-co")
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	plainKey, _, err := st.CreateAPIKey(ctx, tenant.ID, []string{httpapi.ScopeProxyWrite})
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	rm := reservation.NewManager()
	rm.SetBudget(tenant.ID, money.FromFloat(budget))

	// Turn off the risk engine's velocity/circuit machinery so this test
	// isolates the *budget/reservation* invariant specifically, rather
	// than conflating it with THROTTLE/BLOCK decisions from a runaway
	// burst (that behavior is already covered by TestFinalAcceptanceScenario).
	policy := risk.DefaultPolicy()
	policy.VelocityTargetMult = 1e9
	policy.MaxConcurrency = numRequests * 2
	policy.BlockThreshold = 101 // effectively disable score-based BLOCK; budget exhaustion still blocks
	policy.ThrottleThreshold = 101
	re := risk.NewEngine(rm, policy)

	pr := pricing.NewRegistry()
	pr.AddRate(pricing.Rate{
		Provider: "demo-provider", Model: "demo-model",
		InputPerUnit: money.FromFloat(0.0001), OutputPerUnit: money.FromFloat(0.0002), Version: 1,
	})
	mock := provider.NewMockProvider("demo-provider", nil) // fixed ~100/100 units -> ~$0.03/call
	providers := provider.NewRegistry()
	providers.Register(mock)

	l := ledger.New()
	gw := gateway.New(re, rm, pr, providers, l)
	route := gateway.RouteConfig{Route: "/agent/execute", Provider: "demo-provider", Model: "demo-model"}
	srv := httpapi.NewServer(gw, rm, re, l, st, route, "")
	ts := httptest.NewServer(srv)
	defer ts.Close()

	var (
		wg          sync.WaitGroup
		allowedCnt  int
		blockedCnt  int
		otherCnt    int
		mu          sync.Mutex
		maxObserved money.Micros // largest (reserved+settled) seen by any request's own post-call check
	)

	fire := func(i int) {
		defer wg.Done()
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/proxy/x", nil)
		if err != nil {
			t.Errorf("request %d: building request: %v", i, err)
			return
		}
		req.Header.Set("Authorization", "Bearer "+plainKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Errorf("request %d: %v", i, err)
			return
		}
		defer resp.Body.Close()

		exp := rm.Exposure(tenant.ID)
		total := exp.Reserved + exp.Settled

		mu.Lock()
		defer mu.Unlock()
		switch resp.StatusCode {
		case http.StatusOK:
			allowedCnt++
		case http.StatusPaymentRequired, http.StatusForbidden, http.StatusTooManyRequests:
			blockedCnt++
		default:
			otherCnt++
		}
		if total > maxObserved {
			maxObserved = total
		}
	}

	wg.Add(numRequests)
	for i := 0; i < numRequests; i++ {
		go fire(i)
	}
	wg.Wait()

	budgetMicros := money.FromFloat(budget)
	finalExp := rm.Exposure(tenant.ID)
	finalTotal := finalExp.Reserved + finalExp.Settled

	t.Logf("fired %d concurrent requests: allowed=%d blocked/other-rejected=%d unexpected-status=%d",
		numRequests, allowedCnt, blockedCnt, otherCnt)
	t.Logf("final exposure: reserved=%s settled=%s total=%s budget=%s",
		finalExp.Reserved, finalExp.Settled, finalTotal, budgetMicros)
	t.Logf("max (reserved+settled) observed at any point across all goroutines: %s", maxObserved)

	if finalTotal > budgetMicros {
		t.Fatalf("BUDGET INVARIANT VIOLATED: final reserved+settled = %s exceeds budget %s", finalTotal, budgetMicros)
	}
	if maxObserved > budgetMicros {
		t.Fatalf("BUDGET INVARIANT VIOLATED: observed reserved+settled = %s exceeded budget %s during the run", maxObserved, budgetMicros)
	}
	// Sanity: the budget (~$1.00 at ~$0.03/call, ~33 possible allowed
	// calls) must actually have been contended given 150 concurrent
	// requests — otherwise this test isn't proving anything.
	maxPossibleAllowed := int(budgetMicros/money.FromFloat(costPerCall)) + 2 // slack for estimate-vs-actual variance
	if allowedCnt == 0 {
		t.Fatal("expected at least some requests to be allowed before budget exhaustion")
	}
	if allowedCnt > maxPossibleAllowed {
		t.Fatalf("allowed %d requests, which is more than the budget should permit (~%d) — this would itself indicate an overspend", allowedCnt, maxPossibleAllowed)
	}
	if allowedCnt >= numRequests {
		t.Fatalf("all %d requests were allowed — budget was never actually contended, so this test didn't exercise the invariant", numRequests)
	}
}
