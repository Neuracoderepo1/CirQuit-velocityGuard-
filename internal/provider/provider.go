// Package provider defines the upstream provider abstraction (spec section
// 11). This MVP ships two implementations: MockProvider (deterministic,
// no network — used for tests and the demo scenario) and GenericHTTP
// (forwards to a real upstream using an externally supplied cost policy).
package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

type Usage struct {
	InputUnits  int64
	OutputUnits int64
}

type Response struct {
	StatusCode int
	Body       []byte
	Headers    http.Header
}

type ExecRequest struct {
	Method  string
	URL     string
	Headers http.Header
	Body    []byte
}

// Provider executes an upstream call and reports usage for reconciliation.
// Actual dollar cost is computed by the caller via the pricing registry
// from the reported Usage, per spec section 12 (pricing lives in one place).
type Provider interface {
	Name() string
	Execute(ctx context.Context, req ExecRequest) (Response, Usage, error)
}

// MockProvider simulates an upstream for tests and demos without any
// outbound network call. CostFn lets test/demo code script a traffic
// pattern (normal -> runaway -> recovery) deterministically.
type MockProvider struct {
	NameStr string
	UsageFn func(req ExecRequest) Usage
}

func NewMockProvider(name string, usageFn func(req ExecRequest) Usage) *MockProvider {
	return &MockProvider{NameStr: name, UsageFn: usageFn}
}

func (p *MockProvider) Name() string { return p.NameStr }

func (p *MockProvider) Execute(ctx context.Context, req ExecRequest) (Response, Usage, error) {
	u := Usage{InputUnits: 100, OutputUnits: 100}
	if p.UsageFn != nil {
		u = p.UsageFn(req)
	}
	return Response{StatusCode: 200, Body: []byte(`{"mock":true}`)}, u, nil
}

// GenericHTTP forwards to a real upstream base URL. Cost is supplied
// externally (spec section 13: generic HTTP mode does not pretend to know
// provider-specific economics) — here via a flat per-request Usage of 1
// "unit", meant to be combined with a flat RequestFee rate in the pricing
// registry (cost_per_request policy).
type GenericHTTP struct {
	NameStr string
	BaseURL string
	Client  *http.Client

	// AllowedHosts implements the SSRF-protection requirement (spec
	// section 16): only forward to explicitly allowlisted upstream hosts.
	AllowedHosts map[string]bool
}

func NewGenericHTTP(name, baseURL string, allowedHosts []string) *GenericHTTP {
	m := make(map[string]bool, len(allowedHosts))
	for _, h := range allowedHosts {
		m[h] = true
	}
	return &GenericHTTP{
		NameStr:      name,
		BaseURL:      baseURL,
		Client:       &http.Client{Timeout: 30 * time.Second},
		AllowedHosts: m,
	}
}

func (p *GenericHTTP) Name() string { return p.NameStr }

func (p *GenericHTTP) Execute(ctx context.Context, req ExecRequest) (Response, Usage, error) {
	httpReq, err := http.NewRequestWithContext(ctx, req.Method, req.URL, bytes.NewReader(req.Body))
	if err != nil {
		return Response{}, Usage{}, err
	}
	if !p.AllowedHosts[httpReq.URL.Host] {
		return Response{}, Usage{}, fmt.Errorf("provider: host %q is not in the SSRF allowlist for %q", httpReq.URL.Host, p.NameStr)
	}
	httpReq.Header = req.Headers

	resp, err := p.Client.Do(httpReq)
	if err != nil {
		return Response{}, Usage{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return Response{}, Usage{}, err
	}
	// Generic HTTP mode: 1 flat "request" unit; actual $ cost comes from a
	// flat RequestFee rate configured in the pricing registry.
	return Response{StatusCode: resp.StatusCode, Body: body, Headers: resp.Header},
		Usage{InputUnits: 1, OutputUnits: 0}, nil
}

var ErrProviderNotFound = errors.New("provider: not registered")

type Registry struct {
	providers map[string]Provider
}

func NewRegistry() *Registry {
	return &Registry{providers: make(map[string]Provider)}
}

func (r *Registry) Register(p Provider) {
	r.providers[p.Name()] = p
}

func (r *Registry) Get(name string) (Provider, error) {
	p, ok := r.providers[name]
	if !ok {
		return nil, ErrProviderNotFound
	}
	return p, nil
}
