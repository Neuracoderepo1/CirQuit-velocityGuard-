package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// --- limiter unit tests ---

func TestLimiter_AllowsWithinBurst_ThenBlocks(t *testing.T) {
	l := newLimiter(1, 3) // 1 token/sec, burst of 3
	for i := 0; i < 3; i++ {
		if !l.allow("k") {
			t.Fatalf("request %d: expected allow within burst", i)
		}
	}
	if l.allow("k") {
		t.Fatal("expected 4th immediate request to be blocked once burst is exhausted")
	}
}

func TestLimiter_KeysAreIndependent(t *testing.T) {
	l := newLimiter(1, 1)
	if !l.allow("a") {
		t.Fatal("expected first request for key a to be allowed")
	}
	if l.allow("a") {
		t.Fatal("expected second immediate request for key a to be blocked")
	}
	if !l.allow("b") {
		t.Fatal("key b must have its own independent bucket, unaffected by key a")
	}
}

func TestLimiter_NilLimiterAlwaysAllows(t *testing.T) {
	var l *limiter
	for i := 0; i < 100; i++ {
		if !l.allow("anything") {
			t.Fatal("a nil limiter must always allow — this is what makes rate limiting opt-in")
		}
	}
}

// --- HTTP-level integration: rate limiting is off unless explicitly configured ---

func TestServer_NoRateLimitByDefault(t *testing.T) {
	srv, _, _ := newHardeningTestServer(t, "", nil, nil)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// A server constructed without SetRateLimits (as every existing test,
	// including the 150-concurrent-request acceptance test, does) must
	// impose no rate limit at all.
	for i := 0; i < 50; i++ {
		resp, err := http.Get(ts.URL + "/health")
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			t.Fatalf("request %d: got 429 with no rate limit configured", i)
		}
	}
}

func TestServer_IPRateLimit_Enforced(t *testing.T) {
	srv, _, _ := newHardeningTestServer(t, "", nil, nil)
	srv.SetRateLimits(1, 2, 0, 0) // 1 req/s, burst 2, IP-only
	ts := httptest.NewServer(srv)
	defer ts.Close()

	var sawLimited bool
	for i := 0; i < 5; i++ {
		resp, err := http.Get(ts.URL + "/health")
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			sawLimited = true
			break
		}
	}
	if !sawLimited {
		t.Fatal("expected at least one 429 within 5 rapid requests against a burst-2 IP limiter")
	}
}

func TestServer_TenantRateLimit_Enforced(t *testing.T) {
	srv, keyA, _ := newHardeningTestServer(t, "", []string{ScopeReadExposure}, nil)
	srv.SetRateLimits(0, 0, 1, 2) // 1 req/s, burst 2, tenant-only
	ts := httptest.NewServer(srv)
	defer ts.Close()

	var sawLimited bool
	for i := 0; i < 5; i++ {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/exposure", nil)
		req.Header.Set("Authorization", "Bearer "+keyA)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			sawLimited = true
			break
		}
	}
	if !sawLimited {
		t.Fatal("expected at least one 429 within 5 rapid authenticated requests against a burst-2 tenant limiter")
	}
}

func TestServer_TenantRateLimit_DoesNotAffectOtherTenant(t *testing.T) {
	srv, keyA, keyB := newHardeningTestServer(t, "", []string{ScopeReadExposure}, []string{ScopeReadExposure})
	srv.SetRateLimits(0, 0, 1, 1) // burst of exactly 1 per tenant
	ts := httptest.NewServer(srv)
	defer ts.Close()

	get := func(key string) int {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/exposure", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if got := get(keyA); got != http.StatusOK {
		t.Fatalf("tenant A's first request: got %d, want 200", got)
	}
	if got := get(keyA); got != http.StatusTooManyRequests {
		t.Fatalf("tenant A's second immediate request: got %d, want 429", got)
	}
	// Tenant B has never made a request, so its bucket is untouched by A's exhaustion.
	if got := get(keyB); got != http.StatusOK {
		t.Fatalf("tenant B's first request: got %d, want 200 (must not share A's bucket)", got)
	}
}

// --- Legacy unscoped-key audit trail ---

func TestRequireAuth_UnscopedKey_EmitsAuditEvent(t *testing.T) {
	srv, keyA, _ := newHardeningTestServer(t, "", nil, nil) // nil scopes: legacy/unscoped key
	ts := httptest.NewServer(srv)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/exposure", nil)
	req.Header.Set("Authorization", "Bearer "+keyA)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected unscoped key to still pass (legacy compatibility): got %d", resp.StatusCode)
	}

	var body struct {
		TenantID string `json:"tenant_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode /v1/exposure response: %v", err)
	}

	events := srv.Ledger.ForTenant(body.TenantID)
	found := false
	for _, e := range events {
		if string(e.Type) == "LEGACY_UNSCOPED_KEY_USED" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected a LEGACY_UNSCOPED_KEY_USED audit event when an unscoped key passes a scope check")
	}
}
