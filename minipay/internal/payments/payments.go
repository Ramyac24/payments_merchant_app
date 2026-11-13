// Package payments owns the payment lifecycle: creation (with crash-safe
// recovery points), the status state machine, and applying Stripe events.
package payments

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/sindhu/minipay/internal/apierr"
	"github.com/sindhu/minipay/internal/ids"
	"github.com/sindhu/minipay/internal/ledger"
	"github.com/sindhu/minipay/internal/outbox"
	"github.com/sindhu/minipay/internal/store"
	"github.com/sindhu/minipay/internal/stripeapi"
)

type Status string

const (
	StatusCreated    Status = "created"
	StatusProcessing Status = "processing"
	StatusSucceeded  Status = "succeeded"
	StatusFailed     Status = "failed"
	StatusCanceled   Status = "canceled"
)

// Allowed transitions. "failed" is not terminal: in Stripe a PaymentIntent whose
// last attempt failed can be retried with another card and still succeed.
// created -> succeeded/failed covers webhooks racing ahead of our own update.
var transitions = map[Status][]Status{
	StatusCreated:    {StatusProcessing, StatusSucceeded, StatusFailed, StatusCanceled},
	StatusProcessing: {StatusSucceeded, StatusFailed, StatusCanceled},
	StatusFailed:     {StatusProcessing, StatusSucceeded, StatusCanceled},
	StatusSucceeded:  {}, // refunds (milestone 2b) will add succeeded -> refunded
	StatusCanceled:   {},
}

func CanTransition(from, to Status) bool {
	for _, s := range transitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

type Payment struct {
	ID                    string    `json:"id"`
	Object                string    `json:"object"`
	MerchantID            string    `json:"-"`
	Amount                int64     `json:"amount"`
	Currency              string    `json:"currency"`
	Description           string    `json:"description"`
	Status                Status    `json:"status"`
	StripePaymentIntentID *string   `json:"stripe_payment_intent"`
	ClientSecret          *string   `json:"client_secret,omitempty"`
	CreatedAt             time.Time `json:"created_at"`
	UpdatedAt             time.Time `json:"updated_at"`
	Version               int       `json:"-"`
}

type CreateParams struct {
	Amount      int64  `json:"amount"`
	Currency    string `json:"currency"`
	Description string `json:"description"`
}

var supportedCurrencies = map[string]bool{"inr": true, "usd": true, "eur": true, "gbp": true}

const MetadataPaymentID = "minipay_payment_id"

type Service struct {
	DB     *sql.DB
	Stripe stripeapi.Provider
	Log    *slog.Logger
}

func (p CreateParams) validate() error {
	if p.Amount <= 0 {
		return apierr.InvalidRequest("parameter_invalid", "amount", "amount must be a positive integer in minor units")
	}
	if p.Amount > 99_999_999 {
		return apierr.InvalidRequest("amount_too_large", "amount", "amount must be at most 99999999")
	}
	if !supportedCurrencies[p.Currency] {
		return apierr.InvalidRequest("parameter_invalid", "currency", "currency must be one of inr, usd, eur, gbp (lowercase)")
	}
	if len(p.Description) > 500 {
		return apierr.InvalidRequest("parameter_invalid", "description", "description must be at most 500 characters")
	}
	return nil
}

// Create creates a payment and its Stripe PaymentIntent. It is written in three
// phases so that a crash or timeout at any point is recoverable by simply
// retrying with the same idempotency key:
//
//  1. Recovery point: insert the payment row keyed by (merchant, idempotency key).
//     A retry finds the existing row instead of creating a second payment.
//  2. External call: create the PaymentIntent with a Stripe idempotency key
//     derived from our payment id, so Stripe also returns the same intent on retry.
//  3. Record the intent id and move created -> processing, plus an outbox event.
func (s *Service) Create(ctx context.Context, merchantID, idemKey string, p CreateParams) (*Payment, error) {
	p.Currency = strings.ToLower(p.Currency)
	if err := p.validate(); err != nil {
		return nil, err
	}
	if idemKey == "" {
		return nil, errors.New("payments: idempotency key required")
	}

	// Phase 1
	_, err := s.DB.ExecContext(ctx, `INSERT INTO payments
		(id, merchant_id, idempotency_key, amount, currency, description, status)
		VALUES ($1, $2, $3, $4, $5, $6, 'created')
		ON CONFLICT (merchant_id, idempotency_key) DO NOTHING`,
		ids.New("pay"), merchantID, idemKey, p.Amount, p.Currency, p.Description)
	if err != nil {
		return nil, err
	}
	pay, err := s.scanOne(ctx, s.DB, `WHERE merchant_id = $1 AND idempotency_key = $2`, merchantID, idemKey)
	if err != nil {
		return nil, err
	}
	if pay.Amount != p.Amount || pay.Currency != p.Currency || pay.Description != p.Description {
		return nil, apierr.Idempotency(http.StatusUnprocessableEntity, "idempotency_key_reused",
			"this Idempotency-Key was already used with different request parameters")
	}
	if pay.StripePaymentIntentID != nil || pay.Status != StatusCreated {
		return pay, nil // already past phase 2 (an earlier attempt or a webhook got here first)
	}

	// Phase 2
	pi, err := s.Stripe.CreatePaymentIntent(ctx, stripeapi.CreatePaymentIntentParams{
		Amount:         pay.Amount,
		Currency:       pay.Currency,
		Description:    pay.Description,
		Metadata:       map[string]string{MetadataPaymentID: pay.ID, "merchant_id": merchantID},
		IdempotencyKey: "minipay-create-" + pay.ID,
	})
	if err != nil {
		var se *stripeapi.Error
		if errors.As(err, &se) && se.Permanent() {
			if ferr := s.markFailedOnCreate(ctx, pay); ferr != nil {
				return nil, ferr
			}
			return nil, &apierr.Error{Status: http.StatusPaymentRequired, Type: "payment_error",
				Code: se.Code, Message: "processor rejected the payment: " + se.Message}
		}
		s.Log.Warn("stripe create failed; payment left at recovery point", "payment", pay.ID, "err", err)
		return nil, apierr.Upstream("payment processor unavailable; retry with the same Idempotency-Key")
	}

	// Phase 3
	err = store.WithTx(ctx, s.DB, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE payments
			SET stripe_pi_id = $2, client_secret = $3,
			    status = CASE WHEN status = 'created' THEN 'processing' ELSE status END,
			    version = version + 1, updated_at = now()
			WHERE id = $1 AND (stripe_pi_id IS NULL OR stripe_pi_id = $2)`, pay.ID, pi.ID, pi.ClientSecret)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("payment %s is linked to a different PaymentIntent than %s", pay.ID, pi.ID)
		}
		updated, err := s.scanOne(ctx, tx, `WHERE id = $1`, pay.ID)
		if err != nil {
			return err
		}
		_, err = outbox.Insert(ctx, tx, merchantID, pay.ID, "payment.created", updated.public())
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, merchantID, pay.ID)
}

func (s *Service) markFailedOnCreate(ctx context.Context, pay *Payment) error {
	return store.WithTx(ctx, s.DB, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE payments SET status = 'failed', version = version + 1, updated_at = now()
			WHERE id = $1 AND status = 'created'`, pay.ID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		updated, err := s.scanOne(ctx, tx, `WHERE id = $1`, pay.ID)
		if err != nil {
			return err
		}
		_, err = outbox.Insert(ctx, tx, pay.MerchantID, pay.ID, "payment.failed", updated.public())
		return err
	})
}

// Get returns a payment owned by merchantID.
func (s *Service) Get(ctx context.Context, merchantID, id string) (*Payment, error) {
	pay, err := s.scanOne(ctx, s.DB, `WHERE id = $1 AND merchant_id = $2`, id, merchantID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, apierr.NotFound("no such payment: " + id)
	}
	return pay, err
}

// Event outcomes returned by HandleStripeEvent.
const (
	ResultApplied   = "applied"
	ResultDuplicate = "duplicate"
	ResultStale     = "stale_transition"
	ResultIgnored   = "ignored"
)

var eventTargets = map[string]Status{
	"payment_intent.processing":     StatusProcessing,
	"payment_intent.succeeded":      StatusSucceeded,
	"payment_intent.payment_failed": StatusFailed,
	"payment_intent.canceled":       StatusCanceled,
}

// HandleStripeEvent applies a verified Stripe event exactly once. Dedup, the
// status change, ledger postings and the outbox event all commit atomically, so
// a crash mid-way leaves nothing behind and Stripe's retry starts clean.
func (s *Service) HandleStripeEvent(ctx context.Context, ev stripeapi.Event) (string, error) {
	result := ResultApplied
	err := store.WithTx(ctx, s.DB, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO processed_stripe_events (event_id, event_type)
			VALUES ($1, $2) ON CONFLICT DO NOTHING`, ev.ID, ev.Type)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			result = ResultDuplicate
			return nil
		}

		target, handled := eventTargets[ev.Type]
		if !handled {
			result = ResultIgnored
			return nil
		}
		var pi stripeapi.PaymentIntent
		if err := json.Unmarshal(ev.Data.Object, &pi); err != nil {
			return fmt.Errorf("decode payment_intent: %w", err)
		}
		paymentID := pi.Metadata[MetadataPaymentID]
		if paymentID == "" {
			result = ResultIgnored // not created by us (e.g. made in the Stripe dashboard)
			return nil
		}

		pay, err := s.scanOne(ctx, tx, `WHERE id = $1 FOR UPDATE`, paymentID)
		if errors.Is(err, sql.ErrNoRows) {
			// Should not happen; fail so Stripe retries and we get alerted.
			return fmt.Errorf("event %s references unknown payment %s", ev.ID, paymentID)
		}
		if err != nil {
			return err
		}
		if pay.StripePaymentIntentID != nil && *pay.StripePaymentIntentID != pi.ID {
			return fmt.Errorf("event %s: payment %s is linked to %s, not %s", ev.ID, pay.ID, *pay.StripePaymentIntentID, pi.ID)
		}
		if pi.Currency != pay.Currency {
			return fmt.Errorf("event %s: currency mismatch %s vs %s", ev.ID, pi.Currency, pay.Currency)
		}
		if !CanTransition(pay.Status, target) {
			s.Log.Info("ignoring stale/out-of-order event", "event", ev.ID, "type", ev.Type,
				"payment", pay.ID, "from", pay.Status, "to", target)
			result = ResultStale
			return nil
		}

		res, err = tx.ExecContext(ctx, `UPDATE payments
			SET status = $2, stripe_pi_id = COALESCE(stripe_pi_id, $3),
			    version = version + 1, updated_at = now()
			WHERE id = $1 AND version = $4`, pay.ID, target, pi.ID, pay.Version)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("payment %s changed concurrently", pay.ID)
		}

		if target == StatusSucceeded {
			amount := pi.AmountReceived
			if amount == 0 {
				amount = pay.Amount
			}
			// Fees are booked later from Stripe balance transactions (reconciler).
			if _, err := ledger.Post(ctx, tx, pay.ID, "charge succeeded "+pi.ID, []ledger.Entry{
				{Owner: ledger.PlatformOwner, Account: ledger.AccountStripeReceivable, Type: ledger.Asset, Currency: pay.Currency, Amount: amount},
				{Owner: pay.MerchantID, Account: ledger.AccountMerchantPayable, Type: ledger.Liability, Currency: pay.Currency, Amount: -amount},
			}); err != nil {
				return err
			}
		}

		updated, err := s.scanOne(ctx, tx, `WHERE id = $1`, pay.ID)
		if err != nil {
			return err
		}
		_, err = outbox.Insert(ctx, tx, pay.MerchantID, pay.ID, "payment."+string(target), updated.public())
		return err
	})
	return result, err
}

// public strips secrets before a payment is embedded in an outbound event.
func (p *Payment) public() *Payment {
	cp := *p
	cp.ClientSecret = nil
	return &cp
}

type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func (s *Service) scanOne(ctx context.Context, q rowQuerier, where string, args ...any) (*Payment, error) {
	p := Payment{Object: "payment"}
	var status string
	err := q.QueryRowContext(ctx, `SELECT id, merchant_id, amount, currency, description, status,
		stripe_pi_id, client_secret, created_at, updated_at, version FROM payments `+where, args...).
		Scan(&p.ID, &p.MerchantID, &p.Amount, &p.Currency, &p.Description, &status,
			&p.StripePaymentIntentID, &p.ClientSecret, &p.CreatedAt, &p.UpdatedAt, &p.Version)
	if err != nil {
		return nil, err
	}
	p.Status = Status(status)
	return &p, nil
}
