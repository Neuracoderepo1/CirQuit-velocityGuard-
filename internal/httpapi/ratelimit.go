package httpapi

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// limiter is a simple per-key token bucket. Deliberately dependency-free
// (no golang.org/x/time/rate) — the algorithm is the standard textbook
// token bucket: each key's bucket refills continuously at rate
// tokens/sec, capped at burst, and every allowed request costs one token.
//
// A nil *limiter always allows — this is what makes rate limiting opt-in:
// Server fields default to nil so existing callers (tests, and anyone
// constructing a Server directly) get today's unlimited behavior unless
// they explicitly call SetRateLimits.
type limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64 // tokens added per second
	burst   float64 // bucket capacity
	idleTTL time.Duration
}

type bucket struct {
	tokens   float64
	lastSeen time.Time
}

// newLimiter builds a limiter allowing ratePerSec sustained requests per
// key, with bursts up to burst. Both must be positive.
func newLimiter(ratePerSec, burst float64) *limiter {
	l := &limiter{
		buckets: make(map[string]*bucket),
		rate:    ratePerSec,
		burst:   burst,
		idleTTL: 10 * time.Minute,
	}
	go l.sweepLoop()
	return l
}

// allow reports whether the request identified by key may proceed,
// consuming a token if so. Safe for concurrent use.
func (l *limiter) allow(key string) bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, lastSeen: now}
		l.buckets[key] = b
	}
	elapsed := now.Sub(b.lastSeen).Seconds()
	b.tokens += elapsed * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.lastSeen = now

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// sweepLoop periodically evicts buckets that haven't been touched in
// idleTTL, so a limiter keyed on tenant ID or IP doesn't grow without
// bound as new keys are seen over the life of the process.
func (l *limiter) sweepLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		l.mu.Lock()
		cutoff := time.Now().Add(-l.idleTTL)
		for k, b := range l.buckets {
			if b.lastSeen.Before(cutoff) {
				delete(l.buckets, k)
			}
		}
		l.mu.Unlock()
	}
}

// clientIP extracts the caller's address for IP-based rate limiting.
// X-Forwarded-For is honored because this server is expected to run
// behind a platform load balancer (e.g. Render) that sets it on the
// first hop; if you deploy this directly on the public internet without
// a trusted proxy in front of it, this header becomes spoofable and
// should not be trusted for rate-limiting decisions.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
