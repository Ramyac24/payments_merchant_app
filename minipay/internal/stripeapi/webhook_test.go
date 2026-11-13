package stripeapi

import (
	"errors"
	"testing"
	"time"
)

func TestVerifySignature(t *testing.T) {
	payload := []byte(`{"id":"evt_1"}`)
	now := time.Unix(1_700_000_000, 0)
	header := SignatureHeader(payload, "whsec_abc", now)

	if err := VerifySignature(payload, header, "whsec_abc", 5*time.Minute, now.Add(time.Minute)); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	if err := VerifySignature([]byte(`{"id":"evt_2"}`), header, "whsec_abc", 5*time.Minute, now); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("tampered body: %v", err)
	}
	if err := VerifySignature(payload, header, "whsec_other", 5*time.Minute, now); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("wrong secret: %v", err)
	}
	if err := VerifySignature(payload, header, "whsec_abc", 5*time.Minute, now.Add(time.Hour)); !errors.Is(err, ErrTimestamp) {
		t.Fatalf("old timestamp: %v", err)
	}
	if err := VerifySignature(payload, "garbage", "whsec_abc", 5*time.Minute, now); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("garbage header: %v", err)
	}
	// Stripe may send several v1 signatures during secret rotation.
	multi := "t=1700000000,v1=deadbeef," + header[len("t=1700000000,"):]
	if err := VerifySignature(payload, multi, "whsec_abc", 5*time.Minute, now); err != nil {
		t.Fatalf("multi-signature header: %v", err)
	}
}

func TestErrorPermanent(t *testing.T) {
	cases := map[int]bool{400: true, 402: true, 404: true, 409: false, 429: false, 500: false, 503: false}
	for code, want := range cases {
		if got := (&Error{HTTPStatus: code}).Permanent(); got != want {
			t.Errorf("%d: Permanent()=%v, want %v", code, got, want)
		}
	}
}
