// Package ledger is an append-only double-entry ledger.
//
// Sign convention: debit = positive, credit = negative. The entries of every
// transaction must sum to zero. This is checked here and again by a deferred
// constraint trigger in Postgres, so a bug cannot commit unbalanced money.
package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

const PlatformOwner = "platform"

// Well-known accounts.
const (
	// Money Stripe owes the platform (asset). Debited when a charge succeeds.
	AccountStripeReceivable = "stripe_receivable"
	// Money the platform owes a merchant (liability). Credited when a charge succeeds.
	AccountMerchantPayable = "merchant_payable"
)

type AccountType string

const (
	Asset     AccountType = "asset"
	Liability AccountType = "liability"
	Revenue   AccountType = "revenue"
	Expense   AccountType = "expense"
)

type Entry struct {
	Owner    string
	Account  string
	Type     AccountType
	Currency string
	Amount   int64 // +debit / -credit, minor units
}

var ErrUnbalanced = errors.New("ledger: entries do not sum to zero")

// Validate checks the double-entry invariants without touching the database.
func Validate(entries []Entry) error {
	if len(entries) < 2 {
		return errors.New("ledger: a transaction needs at least two entries")
	}
	var sum int64
	currency := entries[0].Currency
	for _, e := range entries {
		if e.Amount == 0 {
			return errors.New("ledger: zero-amount entry")
		}
		if e.Currency != currency {
			return fmt.Errorf("ledger: mixed currencies %q and %q in one transaction", currency, e.Currency)
		}
		sum += e.Amount
	}
	if sum != 0 {
		return fmt.Errorf("%w (sum=%d)", ErrUnbalanced, sum)
	}
	return nil
}

// Post writes a balanced transaction inside the caller's DB transaction and
// returns its id. paymentID may be empty.
func Post(ctx context.Context, tx *sql.Tx, paymentID, description string, entries []Entry) (int64, error) {
	if err := Validate(entries); err != nil {
		return 0, err
	}
	var pid any
	if paymentID != "" {
		pid = paymentID
	}
	var txnID int64
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO ledger_transactions (payment_id, description) VALUES ($1, $2) RETURNING id`,
		pid, description).Scan(&txnID); err != nil {
		return 0, err
	}
	for _, e := range entries {
		accountID, err := ensureAccount(ctx, tx, e)
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO ledger_entries (txn_id, account_id, amount) VALUES ($1, $2, $3)`,
			txnID, accountID, e.Amount); err != nil {
			return 0, err
		}
	}
	return txnID, nil
}

func ensureAccount(ctx context.Context, tx *sql.Tx, e Entry) (int64, error) {
	var id int64
	var typ string
	err := tx.QueryRowContext(ctx, `INSERT INTO ledger_accounts (owner, name, type, currency)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (owner, name, currency) DO UPDATE SET name = EXCLUDED.name
		RETURNING id, type`, e.Owner, e.Account, string(e.Type), e.Currency).Scan(&id, &typ)
	if err != nil {
		return 0, err
	}
	if typ != string(e.Type) {
		return 0, fmt.Errorf("ledger: account %s/%s is %s, not %s", e.Owner, e.Account, typ, e.Type)
	}
	return id, nil
}

type Querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Balance returns the signed balance of an account (0 if it doesn't exist yet).
func Balance(ctx context.Context, q Querier, owner, account, currency string) (int64, error) {
	var bal int64
	err := q.QueryRowContext(ctx, `SELECT COALESCE(SUM(le.amount), 0)
		FROM ledger_entries le JOIN ledger_accounts la ON la.id = le.account_id
		WHERE la.owner = $1 AND la.name = $2 AND la.currency = $3`, owner, account, currency).Scan(&bal)
	return bal, err
}

// TrialBalance returns the sum of every entry in the ledger. It must always be 0.
func TrialBalance(ctx context.Context, q Querier) (int64, error) {
	var sum int64
	err := q.QueryRowContext(ctx, `SELECT COALESCE(SUM(amount), 0) FROM ledger_entries`).Scan(&sum)
	return sum, err
}
