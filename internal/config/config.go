// Package config loads and validates VelocityGuard's runtime configuration
// from environment variables. Per the build directive (section 42):
// secrets live in the environment, never in source, and the process must
// fail fast on invalid configuration rather than start in a half-working
// state.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// StoreMode selects the control-plane persistence backend.
type StoreMode string

const (
	StoreModeMemory   StoreMode = "memory"   // in-process, non-durable (demo/dev)
	StoreModePostgres StoreMode = "postgres" // durable control plane
)

// Config holds all runtime configuration. Add new fields here, not as
// scattered os.Getenv calls elsewhere in the codebase — see rule in
// section 42 ("never scatter... constants across the codebase"), applied
// here to configuration as well as pricing.
type Config struct {
	Addr string // HTTP listen address, e.g. ":8080"

	StoreMode   StoreMode
	PostgresDSN string // required if StoreMode == postgres

	// DemoTenantSlug/DemoBudget seed an initial tenant + budget when
	// running in memory mode with no existing data. Ignored otherwise.
	DemoTenantSlug  string
	DemoBudgetMinor int64 // budget limit in minor currency units (cents)

	// OperatorToken authenticates the kill-switch endpoint. It is never
	// a tenant API key — the kill switch is an operator-only control
	// plane. Required (and must be >=32 chars) in postgres/production
	// mode; optional (with a warning) in memory/demo mode so the local
	// demo binary keeps working without extra setup.
	OperatorToken string

	// UpstreamURL is the single allowlisted destination the proxy is
	// permitted to forward to. Callers can never supply their own
	// destination — see internal/provider.GenericHTTP's AllowedHosts.
	UpstreamURL string
}

// Load reads configuration from the environment and validates it.
// It never panics; callers should treat a non-nil error as fatal and
// exit before binding any listener or opening any connection.
func Load() (Config, error) {
	c := Config{
		Addr:            getEnvDefault("VG_ADDR", ":8080"),
		StoreMode:       StoreMode(strings.ToLower(getEnvDefault("VG_STORE_MODE", "memory"))),
		PostgresDSN:     os.Getenv("VG_POSTGRES_DSN"),
		DemoTenantSlug:  getEnvDefault("VG_DEMO_TENANT_SLUG", "demo-corp"),
		DemoBudgetMinor: 1000, // $10.00 default, overridable below
		OperatorToken:   os.Getenv("VG_OPERATOR_TOKEN"),
		UpstreamURL:     os.Getenv("VG_UPSTREAM_URL"),
	}

	if v := os.Getenv("VG_DEMO_BUDGET_MINOR"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return Config{}, fmt.Errorf("VG_DEMO_BUDGET_MINOR: invalid integer: %w", err)
		}
		c.DemoBudgetMinor = n
	}

	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// Validate checks internal consistency. Kept separate from Load so tests
// can construct a Config directly and validate it without touching the
// environment.
func (c Config) Validate() error {
	if c.Addr == "" {
		return fmt.Errorf("VG_ADDR must not be empty")
	}
	switch c.StoreMode {
	case StoreModeMemory:
		// no further requirements
	case StoreModePostgres:
		if strings.TrimSpace(c.PostgresDSN) == "" {
			return fmt.Errorf("VG_STORE_MODE=postgres requires VG_POSTGRES_DSN to be set")
		}
	default:
		return fmt.Errorf("VG_STORE_MODE must be %q or %q, got %q", StoreModeMemory, StoreModePostgres, c.StoreMode)
	}
	if c.DemoBudgetMinor < 0 {
		return fmt.Errorf("VG_DEMO_BUDGET_MINOR must be >= 0")
	}
	// Postgres mode is the production/durable path — fail fast rather
	// than let the kill switch silently run without operator auth.
	if c.StoreMode == StoreModePostgres && len(c.OperatorToken) < 32 {
		return fmt.Errorf("VG_STORE_MODE=postgres requires VG_OPERATOR_TOKEN to be set and at least 32 characters")
	}
	if c.OperatorToken != "" && len(c.OperatorToken) < 32 {
		return fmt.Errorf("VG_OPERATOR_TOKEN must be at least 32 characters")
	}
	return nil
}

func getEnvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
