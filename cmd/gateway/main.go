// Command gateway starts the VelocityGuard gateway HTTP server with an
// in-memory demo tenant, provider, and pricing configuration.
//
// This is the MVP slice described in the build directive: it proves the
// full request lifecycle (estimate -> reserve -> risk check -> allow /
// throttle / block -> execute -> reconcile) over real HTTP, using a mock
// provider so it runs with zero external dependencies (no Postgres, no
// Redis, no real upstream API needed).
package main

import (
	"context"
	"log"
	"net/http"
	"time"

	"velocityguard/internal/config"
	"velocityguard/internal/gateway"
	"velocityguard/internal/httpapi"
	"velocityguard/internal/ledger"
	"velocityguard/internal/pricing"
	"velocityguard/internal/provider"
	"velocityguard/internal/reservation"
	"velocityguard/internal/risk"
	"velocityguard/internal/store"

	"velocityguard/internal/money"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("invalid configuration: %v", err) // fail fast, per section 42
	}

	ctx := context.Background()
	var st store.Store
	switch cfg.StoreMode {
	case config.StoreModePostgres:
		ps, err := store.OpenPostgresStore(ctx, cfg.PostgresDSN)
		if err != nil {
			log.Fatalf("connecting to postgres control plane: %v", err)
		}
		st = ps
	default:
		st = store.NewMemoryStore()
	}

	// Bootstrap the demo tenant + a fresh API key through the control
	// plane (never a trusted client header) so the golden-path scenario
	// (section 31) is reachable via real HTTP auth.
	tenant, err := st.CreateTenant(ctx, "Demo Corp", cfg.DemoTenantSlug)
	if err != nil {
		log.Fatalf("bootstrapping demo tenant: %v", err)
	}
	plaintextKey, _, err := st.CreateAPIKey(ctx, tenant.ID, []string{"proxy:write", "read:exposure"})
	if err != nil {
		log.Fatalf("bootstrapping demo API key: %v", err)
	}

	rm := reservation.NewManager()
	rm.SetBudget(tenant.ID, money.FromFloat(float64(cfg.DemoBudgetMinor)/100.0))

	re := risk.NewEngine(rm, risk.DefaultPolicy())

	pr := pricing.NewRegistry()
	pr.AddRate(pricing.Rate{
		Provider: "demo-provider", Model: "demo-model",
		InputPerUnit: money.FromFloat(0.0001), OutputPerUnit: money.FromFloat(0.0002),
		Version: 1, EffectiveFrom: time.Unix(0, 0),
	})

	mock := provider.NewMockProvider("demo-provider", nil) // default usage: ~$0.03/call
	providers := provider.NewRegistry()
	providers.Register(mock)

	l := ledger.New()
	gw := gateway.New(re, rm, pr, providers, l)

	route := gateway.RouteConfig{Route: "/agent/execute", Provider: "demo-provider", Model: "demo-model"}
	srv := httpapi.NewServer(gw, rm, re, l, st, route)

	log.Printf("VelocityGuard gateway listening on %s (store mode: %s)", cfg.Addr, cfg.StoreMode)
	log.Printf("demo tenant %q created; API key (shown once, never stored in plaintext):", tenant.Name)
	log.Printf("  %s", plaintextKey)
	log.Printf("try: curl -X POST %s/proxy/x -H 'Authorization: Bearer %s'", cfg.Addr, plaintextKey)
	log.Fatal(http.ListenAndServe(cfg.Addr, srv))
}
