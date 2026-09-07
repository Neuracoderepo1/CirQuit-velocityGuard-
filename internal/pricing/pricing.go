// Package pricing implements a versioned provider pricing registry so that
// historical reconciliation always uses the pricing version that was in
// effect at request time (spec section 12) — a pricing change must never
// silently rewrite past accounting.
package pricing

import (
	"errors"
	"sync"
	"time"

	"velocityguard/internal/money"
)

var ErrNoPricingInEffect = errors.New("pricing: no version in effect at requested time")

// Rate describes cost per unit for a specific provider+model version.
type Rate struct {
	Provider       string
	Model          string
	InputPerUnit   money.Micros // cost per input token/unit
	OutputPerUnit  money.Micros // cost per output token/unit
	RequestFee     money.Micros // flat per-request fee, if any
	Version        int
	EffectiveFrom  time.Time
	EffectiveUntil time.Time // zero value = still in effect
}

func (r Rate) activeAt(t time.Time) bool {
	if t.Before(r.EffectiveFrom) {
		return false
	}
	if !r.EffectiveUntil.IsZero() && !t.Before(r.EffectiveUntil) {
		return false
	}
	return true
}

func (r Rate) Estimate(inputUnits, outputUnits int64) money.Micros {
	return r.RequestFee + money.Micros(inputUnits)*r.InputPerUnit + money.Micros(outputUnits)*r.OutputPerUnit
}

type key struct {
	provider string
	model    string
}

// Registry holds all historical + current rates. Never mutate a Rate in
// place; add a new version with a new EffectiveFrom and close out the old
// one's EffectiveUntil.
type Registry struct {
	mu    sync.RWMutex
	rates map[key][]Rate // sorted by EffectiveFrom ascending
}

func NewRegistry() *Registry {
	return &Registry{rates: make(map[key][]Rate)}
}

// AddRate registers a new pricing version. If a previous version is open-ended
// (EffectiveUntil is zero) and overlaps, it is automatically closed at the
// new version's EffectiveFrom, so history stays gapless and non-overlapping.
func (r *Registry) AddRate(rate Rate) {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := key{rate.Provider, rate.Model}
	list := r.rates[k]
	for i := range list {
		if list[i].EffectiveUntil.IsZero() && !list[i].EffectiveFrom.After(rate.EffectiveFrom) {
			list[i].EffectiveUntil = rate.EffectiveFrom
		}
	}
	list = append(list, rate)
	r.rates[k] = list
}

// RateAt returns the rate in effect for provider/model at time t. Used both
// for live estimation (t = now) and historical reconciliation (t = the
// original request time), per spec section 12.
func (r *Registry) RateAt(provider, model string, t time.Time) (Rate, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	list := r.rates[key{provider, model}]
	for _, rate := range list {
		if rate.activeAt(t) {
			return rate, nil
		}
	}
	return Rate{}, ErrNoPricingInEffect
}
