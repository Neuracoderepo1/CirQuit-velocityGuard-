// Package store defines the control-plane persistence interface for
// tenants and API keys, and provides two implementations: an in-memory
// one (used for tests and the zero-dependency demo binary) and a
// Postgres-backed one (durable, for real deployments).
//
// Per the architecture rule in the build directive (section 4/19):
// nothing in this package sits on the synchronous risk-decision hot
// path. It backs signup/login/key-management flows and is read at
// startup / on demand to authenticate control-plane and gateway
// requests — the risk engine itself never calls this package per
// decision.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

var (
	// ErrNotFound is returned when a lookup finds no matching row.
	ErrNotFound = errors.New("store: not found")
	// ErrRevoked is returned when a key exists but has been revoked.
	ErrRevoked = errors.New("store: key revoked")
	// ErrDuplicateSlug is returned when a tenant slug is already taken.
	ErrDuplicateSlug = errors.New("store: tenant slug already exists")
)

// Tenant is a control-plane organization/account.
type Tenant struct {
	ID        string
	Name      string
	Slug      string
	Status    string // "active" | "suspended"
	CreatedAt time.Time
}

// APIKey is a control-plane record for an issued key. The plaintext
// secret is never stored — only Hash and Prefix, per section 15 of the
// build directive ("Never store plaintext VelocityGuard API keys").
type APIKey struct {
	ID         string
	TenantID   string
	Prefix     string // e.g. "vg_live_ab12cd34", safe to display
	Hash       string // sha256 hex of the full plaintext key
	Scopes     []string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

// Store is the control-plane persistence interface. Both MemoryStore
// and PostgresStore implement it, so tests and the hot-path-adjacent
// auth middleware can be exercised against either without caring which
// backend is active.
type Store interface {
	CreateTenant(ctx context.Context, name, slug string) (Tenant, error)
	GetTenant(ctx context.Context, id string) (Tenant, error)

	// CreateAPIKey generates a new key, stores only its hash, and
	// returns the plaintext secret exactly once. Callers MUST display
	// or transmit it immediately; it cannot be retrieved again.
	CreateAPIKey(ctx context.Context, tenantID string, scopes []string) (plaintext string, key APIKey, err error)

	// Authenticate looks up the tenant owning a given plaintext key.
	// Returns ErrNotFound if no key matches, ErrRevoked if the key was
	// revoked. On success it also updates last_used_at (best-effort;
	// failure to update must not fail the auth check itself).
	Authenticate(ctx context.Context, plaintext string) (Tenant, APIKey, error)

	RevokeAPIKey(ctx context.Context, keyID string) error
}

// keyPrefix is the fixed, non-secret prefix on every issued key, making
// leaked keys grep-able in logs/scanners without revealing anything
// secret. See section 15/16.
const keyPrefix = "vg_live_"

// generatePlaintextKey returns a new random API key and its sha256 hash.
// 32 bytes of crypto/rand entropy, hex-encoded, is 256 bits — far beyond
// brute-force range for a rate-limited auth endpoint.
func generatePlaintextKey() (plaintext, hash, displayPrefix string, err error) {
	buf := make([]byte, 32)
	if _, err = rand.Read(buf); err != nil {
		return "", "", "", fmt.Errorf("generating key entropy: %w", err)
	}
	secret := hex.EncodeToString(buf)
	plaintext = keyPrefix + secret
	h := sha256.Sum256([]byte(plaintext))
	hash = hex.EncodeToString(h[:])
	displayPrefix = plaintext[:len(keyPrefix)+8] // e.g. "vg_live_ab12cd34"
	return plaintext, hash, displayPrefix, nil
}

func hashKey(plaintext string) string {
	h := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(h[:])
}
