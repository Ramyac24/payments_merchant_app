# minipay

**A small payments platform in Go, built on Stripe, that is designed to never charge twice, never lose money, and recover from failures automatically.**

minipay sits between merchants and Stripe. Merchants call a clean REST API; minipay creates Stripe PaymentIntents, consumes Stripe webhooks, tracks every payment through a strict state machine, and records money movement in a double-entry ledger. Every piece is built around the failure modes real payment systems hit: retries, timeouts, crashes, duplicate and out-of-order events.

> **Status:** milestones 1–2 of 5 complete and verified end to end against **Stripe test mode**.
> Next up: outbound webhook dispatcher → reconciliation → observability and Terraform. See the [roadmap](#roadmap).

---

## Highlights

| Problem | How minipay handles it |
|---|---|
| Client retries a request after a timeout | `Idempotency-Key` on every `POST`: the original response is replayed byte for byte (`Idempotent-Replayed: true`), and reusing a key with different params gets a `422` |
| Stripe created the charge, but we crashed or timed out before seeing the response | Payment creation is split into **recovery points**: the DB row is keyed by the idempotency key, and the Stripe call reuses a deterministic Stripe idempotency key, so a retry resumes and never creates a second charge |
| 20 identical requests arrive at the same time | Row-level locking: exactly one payment and one PaymentIntent; the others get `409` or a replay |
| Stripe delivers the same webhook twice | A dedup table keyed by event id, committed in the **same transaction** as the state change |
| Webhooks arrive out of order | A whitelisted state machine; a stale event is recorded and ignored, so status never goes backwards |
| A forged or replayed webhook | HMAC-SHA256 signature check with constant-time compare and a 5-minute timestamp window |
| A bug tries to write an unbalanced ledger entry | Rejected in Go **and** by a deferred Postgres constraint trigger; the ledger is append-only |
| We update state but crash before notifying the merchant | **Transactional outbox**: the event row commits atomically with the state change |

---

## Architecture

```mermaid
flowchart LR
    M[Merchant app] -->|REST + Idempotency-Key| GW
    subgraph minipay
        GW[API gateway<br/>auth · rate limit · idempotency] --> P[Payments service<br/>state machine]
        P --> L[Ledger<br/>double-entry]
        P --> DB[(Postgres<br/>payments · ledger · outbox)]
        L --> DB
        DB -. M3 .-> D[Webhook dispatcher]
        R[Reconciler · M4] -.-> DB
    end
    P <-->|PaymentIntents| S[Stripe]
    S -->|signed webhooks| P
    D -. signed events .-> M
```

### Payment lifecycle

```mermaid
stateDiagram-v2
    [*] --> created
    created --> processing: PaymentIntent created
    created --> failed: Stripe rejected (4xx)
    created --> succeeded: webhook raced ahead
    processing --> succeeded
    processing --> failed: card declined
    failed --> succeeded: retried with another card
    processing --> canceled
    succeeded --> [*]
    canceled --> [*]
```

### Ledger postings

Amounts are integers in minor units (paise or cents); there are no floats anywhere. A debit is positive and a credit is negative, and every transaction sums to zero.

| Event | Account | Entry |
|---|---|---|
| Charge of ₹499 succeeds | `platform / stripe_receivable` (asset) | `+49900` |
| | `merchant / merchant_payable` (liability) | `−49900` |

Full reasoning and trade-offs: **[docs/design.md](docs/design.md)**.

---

## Tech stack

- **Go 1.24**: standard library HTTP (`net/http` routing with method patterns) and `log/slog` structured logging
- **PostgreSQL 16**: transactions, row locks, constraint triggers
- **Stripe API** (test mode), called through a small dependency-free client that also verifies webhook signatures
- **Docker / Docker Compose**: one command brings up the whole stack
- Only one third-party Go dependency: `lib/pq`

---

## Quick start

### Option A: offline demo (no Stripe account needed)

```bash
docker compose up --build -d
docker compose exec api minipayctl create-merchant -name "Demo"
export KEY=mp_test_...                       # paste the printed api_key

curl -s localhost:8080/v1/payments \
  -H "Authorization: Bearer $KEY" \
  -H "Idempotency-Key: order-1001" \
  -d '{"amount": 49900, "currency": "inr", "description": "Pro plan"}'

# send a correctly signed "payment succeeded" webhook for that payment
docker compose exec api minipayctl simulate -payment pay_... -event succeeded
```

Run the same `curl` again: you get the identical response with `Idempotent-Replayed: true`.

### Option B: real Stripe test mode

No real money moves in test mode, and the server **refuses to start with a live key**.

1. Get your **test** secret key (`sk_test_...`) from the Stripe Dashboard: Developers → API keys.
2. Install the Stripe CLI and log in:
   ```bash
   brew install stripe/stripe-cli/stripe
   stripe login
   ```
3. Create a `.env` file in the project root (it is git-ignored):
   ```bash
   MINIPAY_FAKE_STRIPE=0
   STRIPE_SECRET_KEY=sk_test_...
   STRIPE_WEBHOOK_SECRET=whsec_...   # from: stripe listen --api-key $STRIPE_SECRET_KEY --print-secret
   ```
4. Start the API, then forward Stripe's webhooks to it:
   ```bash
   docker compose up -d --force-recreate api
   source .env
   stripe listen --api-key $STRIPE_SECRET_KEY \
     --events payment_intent.succeeded,payment_intent.payment_failed,payment_intent.canceled,payment_intent.processing \
     --forward-to localhost:8080/internal/stripe/webhook
   ```
5. In a second terminal, create a payment and pay it with a test card:
   ```bash
   source .env
   RESP=$(curl -s localhost:8080/v1/payments -H "Authorization: Bearer $KEY" \
     -H "Idempotency-Key: real-001" -d '{"amount": 49900, "currency": "inr"}')
   PAY=$(echo $RESP | sed -E 's/.*"id":"([^"]+)".*/\1/')
   PI=$(echo $RESP | sed -E 's/.*"stripe_payment_intent":"([^"]+)".*/\1/')

   stripe payment_intents confirm $PI -d payment_method=pm_card_visa \
     -d return_url=https://example.com --api-key $STRIPE_SECRET_KEY

   curl -s localhost:8080/v1/payments/$PAY -H "Authorization: Bearer $KEY"   # "status":"succeeded"
   ```

Useful Stripe test payment methods:

| Payment method | Result in minipay |
|---|---|
| `pm_card_visa` | `succeeded`, ledger posted |
| `pm_card_chargeDeclined` | `failed`; confirming again with `pm_card_visa` moves it to `succeeded` |
| amount below about $0.50 | `402 payment_error`, payment marked `failed`, and the rejection is replayed on retry |

---

## API reference

All `/v1` endpoints require `Authorization: Bearer <api_key>`. Every `POST` requires an `Idempotency-Key`.

| Method | Path | Description |
|---|---|---|
| `POST` | `/v1/payments` | Create a payment. Body: `{"amount": int, "currency": "inr\|usd\|eur\|gbp", "description": string}` |
| `GET` | `/v1/payments/{id}` | Retrieve a payment (scoped to the calling merchant; other merchants get a `404`) |
| `POST` | `/internal/stripe/webhook` | Stripe webhook receiver (verifies `Stripe-Signature`) |
| `GET` | `/healthz` | Liveness, including a DB ping |

**Example response**

```json
{
  "id": "pay_592f0f9c93a2b3d51a5f83a8",
  "object": "payment",
  "amount": 49900,
  "currency": "inr",
  "status": "processing",
  "stripe_payment_intent": "pi_3Ub...",
  "client_secret": "pi_3Ub..._secret_...",
  "created_at": "2026-11-13T04:32:30Z"
}
```

**Errors** always share one shape:

```json
{"error": {"type": "invalid_request_error", "code": "parameter_invalid", "param": "amount", "message": "amount must be a positive integer in minor units"}}
```

| Status | Meaning |
|---|---|
| `400` | Validation error, missing `Idempotency-Key`, or bad webhook signature |
| `401` | Missing or invalid API key |
| `402` | Stripe permanently rejected the payment |
| `404` | Not found, or the payment belongs to another merchant |
| `409` | A request with the same key is still in progress |
| `422` | Idempotency key reused with different parameters |
| `429` | Rate limited (per-merchant token bucket); see `Retry-After` |
| `502` | Stripe unavailable; safe to retry with the **same** key |

---

## Testing

```bash
docker compose exec postgres psql -U postgres -c "CREATE DATABASE minipay_test"
MINIPAY_TEST_DATABASE_URL="postgres://postgres:postgres@localhost:5432/minipay_test?sslmode=disable" \
  go test -race -p 1 ./...
```

Integration tests run against **real Postgres** (not mocks) and target failure scenarios rather than happy paths:

| Test | What it proves |
|---|---|
| `TestCreateIsIdempotent` | Same request twice: 1 payment, 1 Stripe call, identical response |
| `TestIdempotencyKeyReusedWithDifferentParams` | Key reuse with a different body gets `422` |
| `TestConcurrentRequestsSameKey` | 20 parallel requests produce exactly one payment and one PaymentIntent |
| `TestRecoveryAfterStripeTimeout` | Stripe succeeded but we timed out: the retry reuses the same intent |
| `TestPermanentStripeErrorMarksFailed` | A 4xx from Stripe gives `failed`, and the `402` is replayed without calling Stripe again |
| `TestSucceededWebhookPostsLedgerExactlyOnce` | A duplicate webhook doesn't double-post; the trial balance stays 0 |
| `TestOutOfOrderEventIsIgnored` | `processing` arriving after `succeeded` doesn't regress the status |
| `TestFailedThenRetriedCardSucceeds` | A decline followed by a successful retry ends in `succeeded` |
| `TestWebhookRacesAheadOfCreate` | A webhook that beats our own DB update is still applied correctly |
| `TestWebhookSignatureChecks` | A wrong secret or stale timestamp gets `400` with no state change |
| `TestLedgerRejectsUnbalancedTransactions` | Unbalanced writes fail, even via raw SQL; entries are append-only |
| `TestRateLimit`, `TestAuthRequired`, `TestGetPaymentIsScopedToMerchant` | Access control and abuse protection |

---

## Inspecting the data

```bash
docker compose exec postgres psql -U postgres minipay
```

```sql
-- payments
SELECT id, amount, status, stripe_pi_id FROM payments ORDER BY created_at DESC;

-- balances per account
SELECT la.owner, la.name, SUM(le.amount)
FROM ledger_entries le JOIN ledger_accounts la ON la.id = le.account_id GROUP BY 1, 2;

-- trial balance: must always be 0
SELECT SUM(amount) FROM ledger_entries;

-- try to tamper with the ledger (fails: append-only)
UPDATE ledger_entries SET amount = 1 WHERE id = 1;
```

---

## Project layout

```
cmd/
  api/                 HTTP server
  minipayctl/          admin CLI: create merchants, simulate signed webhooks
internal/
  api/                 routing, middleware wiring, integration tests
  payments/            create flow (recovery points), state machine, Stripe event handling
  idempotency/         key locking, response replay, stale-lock takeover
  ledger/              double-entry postings and balances
  outbox/              transactional outbox
  stripeapi/           Stripe client, webhook signature verification, in-memory fake
  auth/  ratelimit/    API keys (hashed), per-merchant token bucket
migrations/            SQL schema, embedded and applied at startup
docs/design.md         design doc: decisions, trade-offs, roadmap
```

---

## Troubleshooting (lessons from real testing)

| Symptom | Cause | Fix |
|---|---|---|
| Payment stuck at `processing`, but Stripe shows `succeeded` | `stripe listen` attached to a different account or sandbox than your API key | Always pass `--api-key $STRIPE_SECRET_KEY` to `stripe` commands |
| API log: `rejected stripe webhook ... invalid signature` | `STRIPE_WEBHOOK_SECRET` came from another account | Get it from `stripe listen --api-key $STRIPE_SECRET_KEY --print-secret`, then `docker compose up -d --force-recreate api` |
| `must specify events to forward` | Newer Stripe CLI versions require an event filter | Add `--events payment_intent.succeeded,...` or `--all-snapshot` |
| `No such payment_intent` from the CLI | The CLI's default profile is a different account | Add `--api-key $STRIPE_SECRET_KEY` |
| `amount_too_small` | Stripe's minimum is about $0.50 | Use at least `5000` paise (₹50) |

The stuck-payment case is exactly what the **reconciler (M4)** is designed to detect and repair automatically.

---

## Roadmap

- [x] **M1**: payments API, idempotency, Stripe integration, state machine
- [x] **M2**: double-entry ledger, transactional outbox
- [ ] **M3**: webhook dispatcher: outbox relay, `FOR UPDATE SKIP LOCKED` workers, HMAC-signed delivery, exponential backoff with jitter, dead-letter queue, replay endpoint
- [ ] **M4**: reconciler: match Stripe balance transactions against the ledger, book fees, and flag `missing_internal` / `missing_in_stripe` / `amount_mismatch`
- [ ] **M5**: ops: Prometheus metrics, OpenTelemetry tracing, Grafana dashboard, k6 load test that asserts the trial balance is 0, and Terraform for AWS (RDS + ECS Fargate)
- [ ] Refunds as reversing ledger entries

---
