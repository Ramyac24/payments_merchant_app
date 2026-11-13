-- Milestone 1 + 2 schema: merchants, idempotency, payments, double-entry ledger, outbox.

CREATE TABLE merchants (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- API keys are stored hashed (sha256), never in plaintext.
CREATE TABLE api_keys (
    key_hash    TEXT PRIMARY KEY,
    merchant_id TEXT NOT NULL REFERENCES merchants(id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE idempotency_keys (
    merchant_id   TEXT NOT NULL REFERENCES merchants(id),
    key           TEXT NOT NULL,
    request_hash  TEXT NOT NULL,
    status        TEXT NOT NULL CHECK (status IN ('in_progress', 'completed')),
    response_code INT,
    response_body BYTEA,
    locked_at     TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, key)
);
CREATE INDEX idempotency_keys_created_at ON idempotency_keys (created_at);

CREATE TABLE payments (
    id              TEXT PRIMARY KEY,
    merchant_id     TEXT NOT NULL REFERENCES merchants(id),
    -- The payment row doubles as the "recovery point" for its idempotency key:
    -- a retried request finds this row and resumes instead of creating a new one.
    idempotency_key TEXT NOT NULL,
    amount          BIGINT NOT NULL CHECK (amount > 0),   -- minor units (paise / cents)
    currency        TEXT NOT NULL,
    description     TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL CHECK (status IN ('created', 'processing', 'succeeded', 'failed', 'canceled')),
    stripe_pi_id    TEXT UNIQUE,
    client_secret   TEXT,
    version         INT NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (merchant_id, idempotency_key)
);

-- ---------- Double-entry ledger ----------
-- Sign convention: debit = positive, credit = negative. Every transaction sums to zero.

CREATE TABLE ledger_accounts (
    id       BIGSERIAL PRIMARY KEY,
    owner    TEXT NOT NULL,          -- 'platform' or a merchant id
    name     TEXT NOT NULL,
    type     TEXT NOT NULL CHECK (type IN ('asset', 'liability', 'revenue', 'expense')),
    currency TEXT NOT NULL,
    UNIQUE (owner, name, currency)
);

CREATE TABLE ledger_transactions (
    id          BIGSERIAL PRIMARY KEY,
    payment_id  TEXT REFERENCES payments(id),
    description TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE ledger_entries (
    id         BIGSERIAL PRIMARY KEY,
    txn_id     BIGINT NOT NULL REFERENCES ledger_transactions(id),
    account_id BIGINT NOT NULL REFERENCES ledger_accounts(id),
    amount     BIGINT NOT NULL CHECK (amount <> 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ledger_entries_account ON ledger_entries (account_id);
CREATE INDEX ledger_entries_txn ON ledger_entries (txn_id);

-- Database-level safety net: even if application code has a bug, an unbalanced
-- transaction cannot commit. Deferred so all entries of a txn are inserted first.
CREATE FUNCTION ledger_check_balanced() RETURNS trigger AS $$
BEGIN
    IF (SELECT COALESCE(SUM(amount), 0) FROM ledger_entries WHERE txn_id = NEW.txn_id) <> 0 THEN
        RAISE EXCEPTION 'ledger transaction % is unbalanced', NEW.txn_id;
    END IF;
    RETURN NULL;
END
$$ LANGUAGE plpgsql;

CREATE CONSTRAINT TRIGGER ledger_entries_balanced
    AFTER INSERT ON ledger_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger_check_balanced();

-- The ledger is append-only: corrections are new reversing entries.
CREATE FUNCTION ledger_forbid_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'ledger_entries is append-only';
END
$$ LANGUAGE plpgsql;

CREATE TRIGGER ledger_entries_append_only
    BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_forbid_mutation();

-- ---------- Transactional outbox ----------
-- Written in the same DB transaction as the state change it describes.
-- The webhook dispatcher (milestone 3) relays rows where published_at IS NULL.
CREATE TABLE outbox (
    id           BIGSERIAL PRIMARY KEY,
    event_id     TEXT NOT NULL UNIQUE,
    merchant_id  TEXT NOT NULL REFERENCES merchants(id),
    aggregate_id TEXT NOT NULL,
    event_type   TEXT NOT NULL,
    payload      JSONB NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at TIMESTAMPTZ
);
CREATE INDEX outbox_unpublished ON outbox (id) WHERE published_at IS NULL;

-- Dedup table for incoming Stripe webhooks (Stripe delivers at-least-once).
CREATE TABLE processed_stripe_events (
    event_id     TEXT PRIMARY KEY,
    event_type   TEXT NOT NULL,
    processed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
