// Package httpapi exposes a minimal REST surface over the gateway/risk
// engine so a real HTTP client can drive the system (spec section 24).
// This is intentionally a thin slice of the full API surface described in
// the spec — see README "Implemented vs Not Implemented" for scope.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"velocityguard/internal/gateway"
	"velocityguard/internal/ledger"
	"velocityguard/internal/provider"
	"velocityguard/internal/reservation"
	"velocityguard/internal/risk"
	"velocityguard/internal/store"
)

// tenantCtxKey is an unexported type so context values set by the auth
// middleware can't collide with values set by other packages.
type tenantCtxKey struct{}

type Server struct {
	GW     *gateway.Gateway
	Reserv *reservation.Manager
	Risk   *risk.Engine
	Ledger *ledger.Ledger
	Store  store.Store // control-plane auth; see requireAuth
	Route  gateway.RouteConfig
	mux    *http.ServeMux
}

// NewServer wires up the HTTP surface. store.Store is required: every
// tenant-scoped endpoint authenticates via a real API key rather than
// trusting a client-supplied tenant header (see requireAuth).
func NewServer(gw *gateway.Gateway, rm *reservation.Manager, re *risk.Engine, l *ledger.Ledger, st store.Store, route gateway.RouteConfig) *Server {
	s := &Server{GW: gw, Reserv: rm, Risk: re, Ledger: l, Store: st, Route: route, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) routes() {
	s.mux.HandleFunc("/health", s.handleHealth)
	// Tenant-scoped endpoints require a valid API key. The kill-switch
	// endpoint stays operator-only/unauthenticated in this MVP slice —
	// see docs (TODO) for the planned operator-auth model; it must not
	// ship to production without one.
	s.mux.HandleFunc("/v1/exposure", s.requireAuth(s.handleExposure))
	s.mux.HandleFunc("/v1/events", s.requireAuth(s.handleEvents))
	s.mux.HandleFunc("/v1/kill-switch", s.handleKillSwitch)
	s.mux.HandleFunc("/proxy/", s.requireAuth(s.handleProxy))
}

// requireAuth enforces Bearer-token authentication and injects the
// authenticated tenant ID into the request context. This replaces
// trusting a client-supplied X-VelocityGuard-Tenant header (section
// 15/16: never let the caller assert its own identity) — the tenant
// now comes only from a hashed, revocable key looked up in Store.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(authHeader, prefix) || len(authHeader) <= len(prefix) {
			http.Error(w, "missing or malformed Authorization: Bearer <key> header", http.StatusUnauthorized)
			return
		}
		plaintext := strings.TrimPrefix(authHeader, prefix)

		tenant, _, err := s.Store.Authenticate(r.Context(), plaintext)
		switch {
		case errors.Is(err, store.ErrNotFound):
			http.Error(w, "invalid API key", http.StatusUnauthorized)
			return
		case errors.Is(err, store.ErrRevoked):
			http.Error(w, "API key has been revoked", http.StatusForbidden)
			return
		case err != nil:
			http.Error(w, "authentication failed", http.StatusInternalServerError)
			return
		}

		ctx := context.WithValue(r.Context(), tenantCtxKey{}, tenant.ID)
		next(w, r.WithContext(ctx))
	}
}

// tenantFromContext retrieves the tenant ID set by requireAuth. It must
// only be called from handlers reachable through requireAuth.
func tenantFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(tenantCtxKey{}).(string)
	return id, ok
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleExposure(w http.ResponseWriter, r *http.Request) {
	tenant, _ := tenantFromContext(r.Context()) // set by requireAuth
	exp := s.Reserv.Exposure(tenant)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"tenant_id": tenant,
		"limit":     exp.Limit.Float(),
		"reserved":  exp.Reserved.Float(),
		"settled":   exp.Settled.Float(),
		"available": exp.Available.Float(),
	})
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	// Scoped strictly to the authenticated tenant — never fall back to
	// "all events" here, or tenant isolation (section 29) breaks.
	tenant, _ := tenantFromContext(r.Context())
	events := s.Ledger.ForTenant(tenant)
	writeJSON(w, http.StatusOK, events)
}

func (s *Server) handleKillSwitch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Scope string `json:"scope"` // "global" | "tenant" | "provider"
		Value string `json:"value"` // tenant id or provider name, ignored for global
		On    bool   `json:"on"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	switch body.Scope {
	case "global":
		s.Risk.KillAll(body.On)
	case "tenant":
		s.Risk.KillTenant(body.Value, body.On)
	case "provider":
		s.Risk.KillProvider(body.Value, body.On)
	default:
		http.Error(w, "scope must be global|tenant|provider", http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"scope": body.Scope, "value": body.Value, "on": body.On})
}

// handleProxy is the gateway entrypoint: POST /proxy/{tenant} routes the
// request through estimate->reserve->risk->execute->reconcile.
func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	tenant, _ := tenantFromContext(r.Context()) // set by requireAuth; never trust a client header for identity
	requestID := r.Header.Get("X-Request-ID")
	if requestID == "" {
		requestID = time.Now().UTC().Format(time.RFC3339Nano)
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	result := s.GW.HandleRequest(ctx, requestID, tenant, s.Route,
		provider.ExecRequest{Method: "POST", URL: "mock://demo"}, 100, 100, 1)

	status := http.StatusOK
	switch result.Decision.Action {
	case risk.ActionBlock, risk.ActionKillSwitch:
		status = http.StatusPaymentRequired // 402, signaling financial rejection
	case risk.ActionThrottle:
		status = http.StatusTooManyRequests
	}
	if result.Err != nil && status == http.StatusOK {
		status = http.StatusBadGateway
	}

	writeJSON(w, status, map[string]interface{}{
		"request_id":     result.RequestID,
		"action":         result.Decision.Action,
		"risk_score":     result.Decision.RiskScore,
		"risk_level":     result.Decision.RiskLevel,
		"reason":         result.Decision.Reason,
		"circuit_state":  result.Decision.CircuitState,
		"estimated_cost": result.Decision.EstimatedCost.Float(),
		"actual_cost":    result.ActualCost.Float(),
		"executed":       result.Executed,
	})
}
