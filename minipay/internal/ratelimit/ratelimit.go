// Package ratelimit implements a per-merchant token-bucket rate limiter.
//
// It is in-memory, so limits are per instance. With N replicas the effective
// limit is N x rate; a Redis-backed bucket is the usual next step.
package ratelimit

import (
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/sindhu/minipay/internal/apierr"
	"github.com/sindhu/minipay/internal/auth"
)

type bucket struct {
	tokens float64
	last   time.Time
}

type Limiter struct {
	mu      sync.Mutex
	rate    float64 // tokens added per second
	burst   float64 // bucket capacity
	buckets map[string]*bucket
	now     func() time.Time
}

func New(ratePerSec, burst float64) *Limiter {
	return &Limiter{rate: ratePerSec, burst: burst, buckets: map[string]*bucket{}, now: time.Now}
}

// Allow reports whether one request for key may proceed, consuming a token if so.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	elapsed := now.Sub(b.last).Seconds()
	b.tokens = math.Min(l.burst, b.tokens+elapsed*l.rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Middleware limits by authenticated merchant; it must run after auth.Middleware.
func (l *Limiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !l.Allow(auth.MerchantID(r.Context())) {
			w.Header().Set("Retry-After", "1")
			apierr.Write(w, apierr.RateLimited())
			return
		}
		next.ServeHTTP(w, r)
	})
}
