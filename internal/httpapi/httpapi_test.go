package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"velocityguard/internal/gateway"
	"velocityguard/internal/ledger"
	"velocityguard/internal/money"
	"velocityguard/internal/pricing"
	"velocityguard/internal/provider"
	"velocityguard/internal/reservation"
	"velocityguard/internal/risk"
	"velocityguard/internal/store"
)

// newTestServer builds a fully wired Server backed by MemoryStore, plus
// two tenants with real API keys, for exercising auth over real HTTP
// (httptest), not just calling Go functions directly.
func newTestServer(t *testing.T) (srv *Server, keyA, keyB, tenantAID, tenantBID string) {
	t.Helper()
	ctx := context.Background()
	st := store.NewMemoryStore()

	tenantA, err := st.CreateTenant(ctx, "Tenant A", "tenant-a-"+t.Name())
	if err != nil {
		t.Fatalf("CreateTenant A: %v", err)
	}
	tenantB, err := st.CreateTenant(ctx, "Tenant B", "tenant-b-"+t.Name())
	if err != nil {
		t.Fatalf("CreateTenant B: %v", err)
	}
	plainA, _, err := st.CreateAPIKey(ctx, tenantA.ID, nil)
	if err != nil {
		t.Fatalf("CreateAPIKey A: %v", err)
	}
	plainB, _, err := st.CreateAPIKey(ctx, tenantB.ID, nil)
	if err != nil {
		t.Fatalf("CreateAPIKey B: %v", err)
	}

	rm := reservation.NewManager()
	rm.SetBudget(tenantA.ID, money.FromFloat(10.00))
	rm.SetBudget(tenantB.ID, money.FromFloat(10.00))
	re := risk.NewEngine(rm, risk.DefaultPolicy())
	pr := pricing.NewRegistry()
	pr.AddRate(pricing.Rate{Provider: "demo-provider", Model: "demo-model",
		InputPerUnit: money.FromFloat(0.0001), OutputPerUnit: money.FromFloat(0.0002), Version: 1})
	mock := provider.NewMockProvider("demo-provider", nil)
	providers := provider.NewRegistry()
	providers.Register(mock)
	l := ledger.New()
	gw := gateway.New(re, rm, pr, providers, l)
	route := gateway.RouteConfig{Route: "/agent/execute", Provider: "demo-provider", Model: "demo-model"}

	s := NewServer(gw, rm, re, l, st, route, "")
	return s, plainA, plainB, tenantA.ID, tenantB.ID
}

func TestProxy_NoAuthHeader_Rejected(t *testing.T) {
	srv, _, _, _, _ := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/proxy/x", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("got status %d, want 401", resp.StatusCode)
	}
}

func TestProxy_InvalidKey_Rejected(t *testing.T) {
	srv, _, _, _, _ := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/proxy/x", nil)
	req.Header.Set("Authorization", "Bearer vg_live_not-a-real-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("got status %d, want 401", resp.StatusCode)
	}
}

func TestProxy_ValidKey_Allowed(t *testing.T) {
	srv, keyA, _, _, _ := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/proxy/x", nil)
	req.Header.Set("Authorization", "Bearer "+keyA)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got status %d, want 200", resp.StatusCode)
	}
}

// TestExposure_TenantIsolation is the HTTP-level version of section 29's
// requirement: a valid key for tenant A must never be able to read
// tenant B's financial exposure or events, no matter what the client
// puts in the URL/body — identity comes only from the authenticated key.
func TestExposure_TenantIsolation(t *testing.T) {
	srv, keyA, _, tenantAID, tenantBID := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/exposure?tenant="+tenantBID, nil)
	req.Header.Set("Authorization", "Bearer "+keyA)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got status %d, want 200", resp.StatusCode)
	}

	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["tenant_id"] != tenantAID {
		t.Fatalf("exposure returned tenant_id %v, want %v (a ?tenant= query param must not override the authenticated identity)", body["tenant_id"], tenantAID)
	}
}
