package api_test

// Integration tests against a real Postgres. Run with:
//
//	MINIPAY_TEST_DATABASE_URL=postgres://postgres@localhost:5432/minipay_test?sslmode=disable go test ./...

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/sindhu/minipay/internal/api"
	"github.com/sindhu/minipay/internal/auth"
	"github.com/sindhu/minipay/internal/ledger"
	"github.com/sindhu/minipay/internal/payments"
	"github.com/sindhu/minipay/internal/ratelimit"
	"github.com/sindhu/minipay/internal/store"
	"github.com/sindhu/minipay/internal/stripeapi"
)

const webhookSecret = "whsec_test_secret"

type env struct {
	srv        *httptest.Server
	db         *sql.DB
	fake       *stripeapi.Fake
	apiKey     string
	merchantID string
}

func setup(t *testing.T, limiter *ratelimit.Limiter) *env {
	t.Helper()
	dsn := os.Getenv("MINIPAY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("MINIPAY_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	db, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`TRUNCATE merchants, api_keys, idempotency_keys, payments, ledger_accounts,
		ledger_transactions, ledger_entries, outbox, processed_stripe_events RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	mid, key, err := auth.CreateMerchant(ctx, db, "test merchant")
	if err != nil {
		t.Fatal(err)
	}
	if limiter == nil {
		limiter = ratelimit.New(10_000, 10_000)
	}
	fake := stripeapi.NewFake()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := &api.Server{
		DB:            db,
		Payments:      &payments.Service{DB: db, Stripe: fake, Log: log},
		Limiter:       limiter,
		WebhookSecret: webhookSecret,
		Log:           log,
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &env{srv: srv, db: db, fake: fake, apiKey: key, merchantID: mid}
}

type resp struct {
	status int
	header http.Header
	body   map[string]any
	raw    []byte
}

func (e *env) do(t *testing.T, method, path, apiKey, idemKey string, body any) resp {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	raw, _ := io.ReadAll(r.Body)
	out := resp{status: r.StatusCode, header: r.Header, raw: raw}
	_ = json.Unmarshal(raw, &out.body)
	return out
}

func (e *env) createPayment(t *testing.T, idemKey string, amount int64) resp {
	t.Helper()
	return e.do(t, http.MethodPost, "/v1/payments", e.apiKey, idemKey,
		map[string]any{"amount": amount, "currency": "inr", "description": "order #42"})
}

func (e *env) sendEvent(t *testing.T, eventID, eventType string, pi stripeapi.PaymentIntent, signedAt time.Time, secret string) resp {
	t.Helper()
	obj, _ := json.Marshal(pi)
	body, _ := json.Marshal(map[string]any{
		"id": eventID, "object": "event", "type": eventType, "created": signedAt.Unix(),
		"data": map[string]json.RawMessage{"object": obj},
	})
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/internal/stripe/webhook", bytes.NewReader(body))
	req.Header.Set("Stripe-Signature", stripeapi.SignatureHeader(body, secret, signedAt))
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	raw, _ := io.ReadAll(r.Body)
	out := resp{status: r.StatusCode, raw: raw}
	_ = json.Unmarshal(raw, &out.body)
	return out
}

func piFor(t *testing.T, e *env, paymentID string) stripeapi.PaymentIntent {
	t.Helper()
	var amount int64
	var currency string
	var piID sql.NullString
	if err := e.db.QueryRow(`SELECT amount, currency, stripe_pi_id FROM payments WHERE id = $1`, paymentID).
		Scan(&amount, &currency, &piID); err != nil {
		t.Fatal(err)
	}
	if !piID.Valid {
		piID.String = "pi_from_webhook_race"
	}
	return stripeapi.PaymentIntent{
		ID: piID.String, Object: "payment_intent", Amount: amount, AmountReceived: amount, Currency: currency,
		Metadata: map[string]string{payments.MetadataPaymentID: paymentID},
	}
}

func count(t *testing.T, db *sql.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func status(t *testing.T, db *sql.DB, id string) string {
	t.Helper()
	var s string
	if err := db.QueryRow(`SELECT status FROM payments WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// ---------------------------------------------------------------- API basics

func TestAuthRequired(t *testing.T) {
	e := setup(t, nil)
	if r := e.do(t, http.MethodGet, "/v1/payments/pay_x", "", "", nil); r.status != 401 {
		t.Fatalf("no key: got %d", r.status)
	}
	if r := e.do(t, http.MethodGet, "/v1/payments/pay_x", "mp_test_wrong", "", nil); r.status != 401 {
		t.Fatalf("bad key: got %d", r.status)
	}
}

func TestValidation(t *testing.T) {
	e := setup(t, nil)
	r := e.do(t, http.MethodPost, "/v1/payments", e.apiKey, "k-val", map[string]any{"amount": -5, "currency": "inr"})
	if r.status != 400 || r.body["error"].(map[string]any)["param"] != "amount" {
		t.Fatalf("got %d %s", r.status, r.raw)
	}
	r = e.do(t, http.MethodPost, "/v1/payments", e.apiKey, "k-val2", map[string]any{"amount": 100, "currency": "inr", "surprise": 1})
	if r.status != 400 {
		t.Fatalf("unknown field: got %d %s", r.status, r.raw)
	}
}

func TestMissingIdempotencyKey(t *testing.T) {
	e := setup(t, nil)
	r := e.createPayment(t, "", 1000)
	if r.status != 400 || r.body["error"].(map[string]any)["code"] != "idempotency_key_required" {
		t.Fatalf("got %d %s", r.status, r.raw)
	}
}

func TestGetPaymentIsScopedToMerchant(t *testing.T) {
	e := setup(t, nil)
	r := e.createPayment(t, "k-scope", 1000)
	id := r.body["id"].(string)
	if got := e.do(t, http.MethodGet, "/v1/payments/"+id, e.apiKey, "", nil); got.status != 200 {
		t.Fatalf("owner get: %d", got.status)
	}
	_, otherKey, _ := auth.CreateMerchant(context.Background(), e.db, "other")
	if got := e.do(t, http.MethodGet, "/v1/payments/"+id, otherKey, "", nil); got.status != 404 {
		t.Fatalf("other merchant should get 404, got %d", got.status)
	}
}

func TestRateLimit(t *testing.T) {
	e := setup(t, ratelimit.New(0.0001, 2))
	for i := 0; i < 2; i++ {
		if r := e.do(t, http.MethodGet, "/v1/payments/pay_x", e.apiKey, "", nil); r.status != 404 {
			t.Fatalf("request %d: got %d", i, r.status)
		}
	}
	r := e.do(t, http.MethodGet, "/v1/payments/pay_x", e.apiKey, "", nil)
	if r.status != 429 || r.header.Get("Retry-After") == "" {
		t.Fatalf("expected 429 with Retry-After, got %d", r.status)
	}
}

// ---------------------------------------------------------------- idempotency

func TestCreateIsIdempotent(t *testing.T) {
	e := setup(t, nil)
	first := e.createPayment(t, "k-1", 50000)
	if first.status != 200 || first.body["status"] != "processing" {
		t.Fatalf("first: %d %s", first.status, first.raw)
	}
	second := e.createPayment(t, "k-1", 50000)
	if second.status != 200 || !bytes.Equal(first.raw, second.raw) {
		t.Fatalf("replay differs:\n%s\n%s", first.raw, second.raw)
	}
	if second.header.Get("Idempotent-Replayed") != "true" {
		t.Fatal("replay header missing")
	}
	if n := count(t, e.db, `SELECT count(*) FROM payments`); n != 1 {
		t.Fatalf("payments = %d, want 1", n)
	}
	if e.fake.Calls != 1 {
		t.Fatalf("stripe calls = %d, want 1", e.fake.Calls)
	}
}

func TestIdempotencyKeyReusedWithDifferentParams(t *testing.T) {
	e := setup(t, nil)
	e.createPayment(t, "k-2", 50000)
	r := e.createPayment(t, "k-2", 99999)
	if r.status != 422 || r.body["error"].(map[string]any)["code"] != "idempotency_key_reused" {
		t.Fatalf("got %d %s", r.status, r.raw)
	}
}

// Stripe created the PaymentIntent but our request timed out before we saw the
// response. The retry must reuse that same intent, not create a second one.
func TestRecoveryAfterStripeTimeout(t *testing.T) {
	e := setup(t, nil)
	e.fake.FailAfterCreate = 1
	r := e.createPayment(t, "k-3", 75000)
	if r.status != 502 {
		t.Fatalf("first attempt: want 502, got %d %s", r.status, r.raw)
	}
	r = e.createPayment(t, "k-3", 75000)
	if r.status != 200 || r.body["stripe_payment_intent"] == nil {
		t.Fatalf("retry: %d %s", r.status, r.raw)
	}
	if e.fake.Created() != 1 {
		t.Fatalf("stripe intents = %d, want exactly 1", e.fake.Created())
	}
	if n := count(t, e.db, `SELECT count(*) FROM payments`); n != 1 {
		t.Fatalf("payments = %d, want 1", n)
	}
}

func TestPermanentStripeErrorMarksFailed(t *testing.T) {
	e := setup(t, nil)
	e.fake.Reject = &stripeapi.Error{HTTPStatus: 400, Type: "invalid_request_error", Code: "amount_too_small", Message: "Amount too small"}
	r := e.createPayment(t, "k-4", 1)
	if r.status != 402 {
		t.Fatalf("want 402, got %d %s", r.status, r.raw)
	}
	var id string
	_ = e.db.QueryRow(`SELECT id FROM payments`).Scan(&id)
	if s := status(t, e.db, id); s != "failed" {
		t.Fatalf("status = %s", s)
	}
	again := e.createPayment(t, "k-4", 1)
	if again.status != 402 || again.header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("4xx outcome should be replayed: %d", again.status)
	}
	if e.fake.Calls != 1 {
		t.Fatalf("stripe calls = %d, want 1", e.fake.Calls)
	}
}

func TestConcurrentRequestsSameKey(t *testing.T) {
	e := setup(t, nil)
	const n = 20
	var wg sync.WaitGroup
	results := make([]resp, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = e.createPayment(t, "k-race", 1000)
		}(i)
	}
	wg.Wait()
	var ok int
	ids := map[string]bool{}
	for _, r := range results {
		switch r.status {
		case 200:
			ok++
			ids[r.body["id"].(string)] = true
		case 409:
		default:
			t.Fatalf("unexpected status %d %s", r.status, r.raw)
		}
	}
	if ok == 0 || len(ids) != 1 {
		t.Fatalf("ok=%d distinct ids=%d", ok, len(ids))
	}
	if c := count(t, e.db, `SELECT count(*) FROM payments`); c != 1 {
		t.Fatalf("payments = %d, want 1", c)
	}
	if e.fake.Created() != 1 {
		t.Fatalf("stripe intents = %d", e.fake.Created())
	}
}

// ---------------------------------------------------------------- webhooks

func TestSucceededWebhookPostsLedgerExactlyOnce(t *testing.T) {
	e := setup(t, nil)
	id := e.createPayment(t, "k-5", 100000).body["id"].(string)
	pi := piFor(t, e, id)

	r := e.sendEvent(t, "evt_1", "payment_intent.succeeded", pi, time.Now(), webhookSecret)
	if r.status != 200 || r.body["result"] != payments.ResultApplied {
		t.Fatalf("first delivery: %d %s", r.status, r.raw)
	}
	r = e.sendEvent(t, "evt_1", "payment_intent.succeeded", pi, time.Now(), webhookSecret)
	if r.status != 200 || r.body["result"] != payments.ResultDuplicate {
		t.Fatalf("redelivery: %d %s", r.status, r.raw)
	}

	if s := status(t, e.db, id); s != "succeeded" {
		t.Fatalf("status = %s", s)
	}
	ctx := context.Background()
	recv, _ := ledger.Balance(ctx, e.db, ledger.PlatformOwner, ledger.AccountStripeReceivable, "inr")
	payable, _ := ledger.Balance(ctx, e.db, e.merchantID, ledger.AccountMerchantPayable, "inr")
	trial, _ := ledger.TrialBalance(ctx, e.db)
	if recv != 100000 || payable != -100000 || trial != 0 {
		t.Fatalf("receivable=%d payable=%d trial=%d", recv, payable, trial)
	}
	if n := count(t, e.db, `SELECT count(*) FROM outbox WHERE aggregate_id = $1`, id); n != 2 {
		t.Fatalf("outbox events = %d, want 2 (created, succeeded)", n)
	}
	if n := count(t, e.db, `SELECT count(*) FROM outbox WHERE payload::text LIKE '%secret%'`); n != 0 {
		t.Fatal("client_secret leaked into outbox payload")
	}
}

func TestOutOfOrderEventIsIgnored(t *testing.T) {
	e := setup(t, nil)
	id := e.createPayment(t, "k-6", 2000).body["id"].(string)
	pi := piFor(t, e, id)
	e.sendEvent(t, "evt_a", "payment_intent.succeeded", pi, time.Now(), webhookSecret)
	r := e.sendEvent(t, "evt_b", "payment_intent.processing", pi, time.Now(), webhookSecret)
	if r.body["result"] != payments.ResultStale {
		t.Fatalf("got %s", r.raw)
	}
	if s := status(t, e.db, id); s != "succeeded" {
		t.Fatalf("status regressed to %s", s)
	}
}

func TestFailedThenRetriedCardSucceeds(t *testing.T) {
	e := setup(t, nil)
	id := e.createPayment(t, "k-7", 2000).body["id"].(string)
	pi := piFor(t, e, id)
	e.sendEvent(t, "evt_f", "payment_intent.payment_failed", pi, time.Now(), webhookSecret)
	if s := status(t, e.db, id); s != "failed" {
		t.Fatalf("status = %s", s)
	}
	e.sendEvent(t, "evt_s", "payment_intent.succeeded", pi, time.Now(), webhookSecret)
	if s := status(t, e.db, id); s != "succeeded" {
		t.Fatalf("status = %s", s)
	}
}

// The webhook can arrive before our own create request has recorded the intent id.
func TestWebhookRacesAheadOfCreate(t *testing.T) {
	e := setup(t, nil)
	e.fake.FailAfterCreate = 1
	e.createPayment(t, "k-8", 3000) // 502: payment stuck at the recovery point
	var id string
	_ = e.db.QueryRow(`SELECT id FROM payments`).Scan(&id)

	pi := piFor(t, e, id) // no stripe_pi_id stored yet
	r := e.sendEvent(t, "evt_race", "payment_intent.succeeded", pi, time.Now(), webhookSecret)
	if r.body["result"] != payments.ResultApplied {
		t.Fatalf("got %s", r.raw)
	}
	retry := e.createPayment(t, "k-8", 3000)
	if retry.status != 200 || retry.body["status"] != "succeeded" {
		t.Fatalf("retry: %d %s", retry.status, retry.raw)
	}
	if e.fake.Calls != 1 {
		t.Fatalf("retry should not call stripe again once the webhook linked the intent; calls=%d", e.fake.Calls)
	}
}

func TestWebhookSignatureChecks(t *testing.T) {
	e := setup(t, nil)
	id := e.createPayment(t, "k-9", 2000).body["id"].(string)
	pi := piFor(t, e, id)
	if r := e.sendEvent(t, "evt_x", "payment_intent.succeeded", pi, time.Now(), "whsec_wrong"); r.status != 400 {
		t.Fatalf("wrong secret: %d", r.status)
	}
	if r := e.sendEvent(t, "evt_y", "payment_intent.succeeded", pi, time.Now().Add(-10*time.Minute), webhookSecret); r.status != 400 {
		t.Fatalf("replayed old event: %d", r.status)
	}
	if s := status(t, e.db, id); s != "processing" {
		t.Fatalf("rejected webhooks must not change state; status = %s", s)
	}
}

// ---------------------------------------------------------------- ledger invariants

func TestLedgerRejectsUnbalancedTransactions(t *testing.T) {
	e := setup(t, nil)
	ctx := context.Background()

	err := store.WithTx(ctx, e.db, func(tx *sql.Tx) error {
		_, err := ledger.Post(ctx, tx, "", "bad", []ledger.Entry{
			{Owner: "platform", Account: "a", Type: ledger.Asset, Currency: "inr", Amount: 100},
			{Owner: "platform", Account: "b", Type: ledger.Liability, Currency: "inr", Amount: -99},
		})
		return err
	})
	if !errors.Is(err, ledger.ErrUnbalanced) {
		t.Fatalf("app-level check: got %v", err)
	}

	// Bypass the Go check entirely: the DB trigger must still refuse to commit.
	err = store.WithTx(ctx, e.db, func(tx *sql.Tx) error {
		var txn, acct int64
		if err := tx.QueryRow(`INSERT INTO ledger_transactions (description) VALUES ('raw') RETURNING id`).Scan(&txn); err != nil {
			return err
		}
		if err := tx.QueryRow(`INSERT INTO ledger_accounts (owner, name, type, currency)
			VALUES ('platform', 'raw', 'asset', 'inr') RETURNING id`).Scan(&acct); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO ledger_entries (txn_id, account_id, amount) VALUES ($1, $2, 500)`, txn, acct)
		return err
	})
	if err == nil {
		t.Fatal("DB accepted an unbalanced transaction")
	}

	// Entries are append-only.
	id := e.createPayment(t, "k-10", 100).body["id"].(string)
	e.sendEvent(t, "evt_l", "payment_intent.succeeded", piFor(t, e, id), time.Now(), webhookSecret)
	if _, err := e.db.Exec(`UPDATE ledger_entries SET amount = amount + 1`); err == nil {
		t.Fatal("ledger entries were mutable")
	}
}
