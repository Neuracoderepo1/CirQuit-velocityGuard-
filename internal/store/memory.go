package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// MemoryStore is a thread-safe, non-durable Store implementation. It
// backs the zero-dependency demo binary (main.go) and is also used
// directly in unit tests so the same test suite (see store_test.go)
// can run against it and, when VG_TEST_POSTGRES_DSN is set, against a
// real Postgres instance too.
type MemoryStore struct {
	mu      sync.RWMutex
	tenants map[string]Tenant
	slugs   map[string]string // slug -> tenant id
	keys    map[string]APIKey // key hash -> APIKey
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		tenants: make(map[string]Tenant),
		slugs:   make(map[string]string),
		keys:    make(map[string]APIKey),
	}
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (m *MemoryStore) CreateTenant(_ context.Context, name, slug string) (Tenant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.slugs[slug]; exists {
		return Tenant{}, ErrDuplicateSlug
	}
	t := Tenant{
		ID:        newID(),
		Name:      name,
		Slug:      slug,
		Status:    "active",
		CreatedAt: time.Now().UTC(),
	}
	m.tenants[t.ID] = t
	m.slugs[slug] = t.ID
	return t, nil
}

func (m *MemoryStore) GetTenant(_ context.Context, id string) (Tenant, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.tenants[id]
	if !ok {
		return Tenant{}, ErrNotFound
	}
	return t, nil
}

func (m *MemoryStore) CreateAPIKey(_ context.Context, tenantID string, scopes []string) (string, APIKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.tenants[tenantID]; !ok {
		return "", APIKey{}, ErrNotFound
	}

	plaintext, hash, prefix, err := generatePlaintextKey()
	if err != nil {
		return "", APIKey{}, err
	}
	key := APIKey{
		ID:        newID(),
		TenantID:  tenantID,
		Prefix:    prefix,
		Hash:      hash,
		Scopes:    scopes,
		CreatedAt: time.Now().UTC(),
	}
	m.keys[hash] = key
	return plaintext, key, nil
}

func (m *MemoryStore) Authenticate(_ context.Context, plaintext string) (Tenant, APIKey, error) {
	hash := hashKey(plaintext)

	m.mu.RLock()
	key, ok := m.keys[hash]
	m.mu.RUnlock()
	if !ok {
		return Tenant{}, APIKey{}, ErrNotFound
	}
	if key.RevokedAt != nil {
		return Tenant{}, APIKey{}, ErrRevoked
	}

	m.mu.RLock()
	tenant, ok := m.tenants[key.TenantID]
	m.mu.RUnlock()
	if !ok {
		return Tenant{}, APIKey{}, ErrNotFound
	}

	// Best-effort last-used update; never fails the auth check.
	now := time.Now().UTC()
	m.mu.Lock()
	key.LastUsedAt = &now
	m.keys[hash] = key
	m.mu.Unlock()

	return tenant, key, nil
}

func (m *MemoryStore) RevokeAPIKey(_ context.Context, keyID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for hash, k := range m.keys {
		if k.ID == keyID {
			now := time.Now().UTC()
			k.RevokedAt = &now
			m.keys[hash] = k
			return nil
		}
	}
	return ErrNotFound
}

var _ Store = (*MemoryStore)(nil)
