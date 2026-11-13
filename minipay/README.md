# minipay

A small payments platform in Go, built to explore the problems payment companies actually care about: **never charging twice, never losing money, and surviving failures.**

It sits on top of Stripe (test mode) and adds:

- **Idempotent API**: every `POST` takes an `Idempotency-Key`. Retries replay the original response, and reusing a key with different params is rejected.
- **Crash-safe payment creation**: a DB "recovery point" plus a deterministic Stripe idempotency key mean a timeout mid-request never creates a duplicate charge.
- **Payment state machine**: explicit allowed transitions. Out-of-order and duplicate Stripe webhooks are handled safely.
- **Double-entry ledger**: append-only, integer minor units. Balance is enforced in Go *and* by a deferred Postgres constraint trigger.
- **Transactional outbox**: state change, ledger postings and the outbound event commit in one transaction.
- **Per-merchant token-bucket rate limiting**, hashed API keys, and Stripe-style error objects.

> Status: milestones 1–2 of 5 done (API, idempotency, Stripe integration, ledger, outbox).
> Next: webhook dispatcher → reconciler → observability + Terraform. See [docs/design.md](docs/design.md).

## Quick start (no Stripe account needed)

```bash
make up                                   # Postgres + API with a fake Stripe
docker compose exec api minipayctl create-merchant -name "Demo"
# copy the api_key it prints

curl -s localhost:8080/v1/payments \
  -H "Authorization: Bearer $KEY" \
  -H "Idempotency-Key: order-1001" \
  -d '{"amount": 49900, "currency": "inr", "description": "Pro plan"}'

# run it again: same response, header `Idempotent-Replayed: true`

# simulate Stripe telling us the charge succeeded (signed webhook)
docker compose exec api minipayctl simulate -payment pay_... -event succeeded
```

## With real Stripe test mode

```bash
export STRIPE_SECRET_KEY=sk_test_...        # live keys are refused at startup
stripe listen --forward-to localhost:8080/internal/stripe/webhook
export STRIPE_WEBHOOK_SECRET=whsec_...      # printed by `stripe listen`
make db && make run
```

Confirm the PaymentIntent with a test card (e.g. via `stripe payment_intents confirm pi_... --payment-method pm_card_visa`). The `payment_intent.succeeded` webhook flows back and the payment moves to `succeeded`.

## API

| Method | Path | Notes |
|---|---|---|
| `POST` | `/v1/payments` | `{amount, currency, description}`; requires `Idempotency-Key` |
| `GET` | `/v1/payments/{id}` | scoped to the calling merchant |
| `POST` | `/internal/stripe/webhook` | verifies `Stripe-Signature` (HMAC-SHA256, 5-min tolerance) |
| `GET` | `/healthz` | DB liveness |

Errors always look like this:

```json
{"error": {"type": "invalid_request_error", "code": "parameter_invalid", "param": "amount", "message": "..."}}
```

## Tests

```bash
make db && make test
```

The integration tests run against real Postgres and cover the failure modes:

| Scenario | Guarantee |
|---|---|
| Same request sent twice | One payment, one Stripe call, identical response |
| Key reused with different body | 422 |
| 20 concurrent requests, same key | Exactly one payment and one PaymentIntent |
| Stripe created the intent but we timed out | Retry reuses the *same* intent |
| Stripe rejects permanently (4xx) | Payment marked `failed`; the 402 is replayed, not retried |
| Duplicate webhook delivery | Ledger posted once |
| Out-of-order webhook (`processing` after `succeeded`) | Ignored; status never regresses |
| Card fails, customer retries and succeeds | `failed → succeeded` allowed |
| Webhook arrives before our own create finishes | Applied correctly; retry doesn't call Stripe again |
| Bad or stale signature | 400, no state change |
| Unbalanced ledger write (even via raw SQL) | Commit refused by DB trigger |
| `UPDATE` on ledger entries | Refused (append-only) |

## Layout

```
cmd/api            HTTP server
cmd/minipayctl     admin CLI: create merchants, simulate signed webhooks
internal/api       routes + middleware wiring, integration tests
internal/payments  create flow (recovery points), state machine, event handling
internal/idempotency  key locking, replay, stale-lock takeover
internal/ledger    double-entry postings and balances
internal/outbox    transactional outbox
internal/stripeapi dependency-free Stripe client, webhook verification, fake
migrations/        SQL schema (embedded, applied at startup)
```
