# minipay design doc

## Goals

1. A merchant can never be charged twice for one logical request, whatever retries, timeouts or crashes happen.
2. Money is never created or destroyed: every ledger transaction balances, and the ledger is append-only.
3. Every state change is eventually announced to the merchant exactly once *from our side* (at-least-once delivery, deduplicated by event id).
4. Failures are boring: any step can crash and the system converges once retried.

Non-goals (for now): multi-region, refunds and disputes, payouts, PCI scope (Stripe holds card data).

## Architecture

```
merchant ──► API gateway (auth → rate limit → idempotency) ──► payments service ──► Stripe
                                                                   │      ▲
                                                                   ▼      │ webhooks
                                         Postgres: payments · ledger · outbox · dedup
                                                                   │
                                         webhook dispatcher (M3) ◄─┘      reconciler (M4)
```

## Key decisions

### 1. Two layers of idempotency

**HTTP layer** (`internal/idempotency`): the key is inserted as `in_progress` with `INSERT … ON CONFLICT DO NOTHING`. The winner runs the handler and stores the response. Losers get a replay, a 409 (still running) or a 422 (different params, detected via a request hash). A lock older than 60s is assumed to belong to a crashed process and is taken over. On a 5xx the key is *released*, so the client can retry.

**Domain layer** (`internal/payments.Create`): releasing on 5xx is only safe if the handler can be re-run. So creation is split into phases around a recovery point:

1. Insert the payment row, unique on `(merchant_id, idempotency_key)`. A retry finds this row instead of creating a new one.
2. Call Stripe with `Idempotency-Key: minipay-create-<payment_id>`. On retry, Stripe returns the *same* PaymentIntent.
3. Record the intent id and move `created → processing`, plus an outbox event, in one transaction.

A crash between any two phases is fixed by the next retry. This is the case most designs get wrong: "Stripe succeeded, but we timed out before seeing the response."

### 2. State machine with conditional updates

```
created ──► processing ──► succeeded
   │             │  ▲
   │             ▼  │
   └─────────► failed ──► (retry with another card)
   └──────────────────────► canceled
```

Transitions are whitelisted in code. Event application locks the row (`SELECT … FOR UPDATE`) and updates with `WHERE version = $n`. An event that would move the payment backwards (e.g. `processing` arriving after `succeeded`) is recorded as processed and ignored, never applied. `created → succeeded` is allowed because Stripe's webhook can beat our own phase-3 update.

### 3. Exactly-once effects from at-least-once webhooks

Stripe redelivers events. A single transaction does:

1. Insert into `processed_stripe_events` (PK = event id). If it conflicts, it's a duplicate, so stop.
2. Validate the transition.
3. Update the payment.
4. Post ledger entries.
5. Insert the outbox event.

All of it commits or none of it does. A crash mid-way rolls back the dedup row too, so Stripe's retry is processed normally.

### 4. Ledger

- Debit = positive, credit = negative, in integer minor units (no floats, ever).
- A successful charge debits `platform/stripe_receivable` and credits `<merchant>/merchant_payable`. Stripe fees will be booked by the reconciler from balance transactions, because the fee isn't known at `payment_intent.succeeded` time.
- Invariants are enforced twice: `ledger.Validate` in Go, and a `DEFERRABLE INITIALLY DEFERRED` constraint trigger that rejects any transaction whose entries don't sum to zero, even ones written by raw SQL. A `BEFORE UPDATE OR DELETE` trigger makes entries append-only.

### 5. Transactional outbox

Publishing to a queue after committing has a gap: crash after the commit, and the event is lost. Publishing before committing means announcing something that may roll back. The outbox row is written inside the business transaction, and a separate relay (M3) delivers it. Outbound payloads strip `client_secret`.

### 6. Security

- API keys are random 192-bit strings, stored as SHA-256 hashes.
- Payments are always queried with `merchant_id`, so other merchants get a 404, not a 403. That avoids leaking which ids exist.
- Webhooks are verified with HMAC-SHA256 over `"<t>.<body>"`, compared in constant time, with a 5-minute window against replay. Multiple `v1` signatures are supported for secret rotation.
- The server refuses to start with a live Stripe key.

## Trade-offs and known limits

| Choice | Why | Cost / next step |
|---|---|---|
| Postgres for idempotency keys | Same transactional store, simple | Extra write per POST; could move to Redis with a TTL at scale |
| In-memory rate limiter | Zero dependencies | Per-instance limits; Redis token bucket for multiple replicas |
| Outbox polling (M3) | Simple, no extra infra | Latency ≈ poll interval; CDC (Debezium) or LISTEN/NOTIFY later |
| Synchronous Stripe call in request path | Simple API for merchants | Slow Stripe means slow API; bounded by a 30s timeout + 2 retries |

## Roadmap

- **M3 – Webhook dispatcher**: outbox relay → `webhook_deliveries`, workers using `FOR UPDATE SKIP LOCKED`, HMAC-signed requests, exponential backoff with jitter (~8 attempts over ~3 days), DLQ, and a replay endpoint.
- **M4 – Reconciler**: page through Stripe balance transactions, match on PaymentIntent id, book fees, and report `missing_internal` / `missing_in_stripe` / `amount_mismatch`.
- **M5 – Ops**: Prometheus metrics (request latency, webhook lag, recon mismatches), OpenTelemetry traces, Grafana dashboard, k6 load test that asserts the trial balance is 0 afterwards, and Terraform for AWS (RDS + ECS Fargate).
- **Refunds**: `succeeded → partially_refunded/refunded`, booked as reversing ledger entries.
