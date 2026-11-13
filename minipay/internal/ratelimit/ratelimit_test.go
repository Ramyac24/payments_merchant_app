package ratelimit

import (
	"testing"
	"time"
)

func TestTokenBucket(t *testing.T) {
	now := time.Unix(0, 0)
	l := New(2, 3) // 2 tokens/sec, burst 3
	l.now = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		if !l.Allow("m") {
			t.Fatalf("burst request %d denied", i)
		}
	}
	if l.Allow("m") {
		t.Fatal("4th request should be limited")
	}
	if !l.Allow("other") {
		t.Fatal("buckets must be per key")
	}
	now = now.Add(500 * time.Millisecond) // refills 1 token
	if !l.Allow("m") {
		t.Fatal("token should have refilled")
	}
	if l.Allow("m") {
		t.Fatal("only one token should have refilled")
	}
	now = now.Add(time.Hour) // refill caps at burst
	for i := 0; i < 3; i++ {
		l.Allow("m")
	}
	if l.Allow("m") {
		t.Fatal("refill must cap at burst")
	}
}
