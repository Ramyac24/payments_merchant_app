package stripeapi

import (
	"context"
	"errors"
	"sync"

	"github.com/sindhu/minipay/internal/ids"
)

// Fake is an in-memory Provider that honours idempotency keys like Stripe does.
// It powers the test suite and the offline demo mode (MINIPAY_FAKE_STRIPE=1).
type Fake struct {
	mu    sync.Mutex
	byKey map[string]*PaymentIntent
	Calls int

	// FailAfterCreate makes the next N calls create the PaymentIntent but then
	// return an error, simulating a timeout where Stripe succeeded but we never
	// saw the response. This is the scenario idempotency exists for.
	FailAfterCreate int
	// FailBeforeCreate makes the next N calls fail without creating anything.
	FailBeforeCreate int
	// Reject makes every call return a permanent (4xx) error.
	Reject *Error
}

func NewFake() *Fake { return &Fake{byKey: map[string]*PaymentIntent{}} }

var ErrFakeTimeout = errors.New("fake stripe: request timed out")

func (f *Fake) CreatePaymentIntent(_ context.Context, p CreatePaymentIntentParams) (*PaymentIntent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls++
	if f.Reject != nil {
		return nil, f.Reject
	}
	if f.FailBeforeCreate > 0 {
		f.FailBeforeCreate--
		return nil, ErrFakeTimeout
	}
	pi, ok := f.byKey[p.IdempotencyKey]
	if !ok {
		id := ids.New("pi_fake")
		pi = &PaymentIntent{
			ID: id, Object: "payment_intent", Amount: p.Amount, Currency: p.Currency,
			Status: "requires_payment_method", ClientSecret: id + "_secret_" + ids.Random(8),
			Metadata: p.Metadata,
		}
		if p.IdempotencyKey != "" {
			f.byKey[p.IdempotencyKey] = pi
		}
	}
	if f.FailAfterCreate > 0 {
		f.FailAfterCreate--
		return nil, ErrFakeTimeout
	}
	cp := *pi
	return &cp, nil
}

// Created returns how many distinct PaymentIntents exist.
func (f *Fake) Created() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.byKey)
}
