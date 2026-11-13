// Package idempotency makes POST requests safe to retry.
//
// Algorithm (per merchant + Idempotency-Key):
//   - INSERT the key as 'in_progress'. If we win the insert, we own the request.
//   - Key exists with a different request hash -> 422 (key reused with other params).
//   - Key exists and is 'completed'          -> replay the stored response verbatim.
//   - Key exists and is 'in_progress'        -> 409, unless the lock is stale
//     (owner crashed), in which case we take it over.
//   - Handler returned 5xx -> release the key so the client can retry. Handlers
//     must therefore be safe to re-run (payments uses a DB recovery point for this).
package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/sindhu/minipay/internal/apierr"
	"github.com/sindhu/minipay/internal/auth"
)

const (
	HeaderKey      = "Idempotency-Key"
	HeaderReplayed = "Idempotent-Replayed"
	maxKeyLen      = 255
	maxBody        = 1 << 20
)

type ctxKey struct{}

// KeyFromContext returns the idempotency key of the current request.
func KeyFromContext(ctx context.Context) string {
	k, _ := ctx.Value(ctxKey{}).(string)
	return k
}

type Middleware struct {
	DB *sql.DB
	// LockTimeout is how long an in_progress key is honoured before another
	// request may take it over (protects against crashed owners).
	LockTimeout time.Duration
}

type record struct {
	requestHash  string
	status       string
	responseCode sql.NullInt64
	responseBody []byte
}

func (m Middleware) Wrap(next http.Handler) http.Handler {
	lockTimeout := m.LockTimeout
	if lockTimeout == 0 {
		lockTimeout = time.Minute
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		key := r.Header.Get(HeaderKey)
		if key == "" {
			apierr.Write(w, apierr.InvalidRequest("idempotency_key_required", HeaderKey,
				"POST requests require an Idempotency-Key header"))
			return
		}
		if len(key) > maxKeyLen {
			apierr.Write(w, apierr.InvalidRequest("idempotency_key_invalid", HeaderKey, "Idempotency-Key is too long"))
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
		if err != nil {
			apierr.Write(w, apierr.InvalidRequest("body_unreadable", "", "could not read request body"))
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))

		ctx := r.Context()
		merchantID := auth.MerchantID(ctx)
		hash := requestHash(r.Method, r.URL.Path, body)

		acquired, rec, err := m.acquire(ctx, merchantID, key, hash, lockTimeout)
		if err != nil {
			apierr.Write(w, err)
			return
		}
		if !acquired {
			switch {
			case rec.requestHash != hash:
				apierr.Write(w, apierr.Idempotency(http.StatusUnprocessableEntity, "idempotency_key_reused",
					"this Idempotency-Key was already used with different request parameters"))
			case rec.status == "completed":
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set(HeaderReplayed, "true")
				w.WriteHeader(int(rec.responseCode.Int64))
				_, _ = w.Write(rec.responseBody)
			default:
				apierr.Write(w, apierr.Idempotency(http.StatusConflict, "idempotency_request_in_progress",
					"a request with this Idempotency-Key is still being processed; retry shortly"))
			}
			return
		}

		rw := &recorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r.WithContext(context.WithValue(ctx, ctxKey{}, key)))

		// Persist with a fresh context: the client may have disconnected, but we
		// still must record the outcome.
		saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if rw.status >= 500 {
			_, err = m.DB.ExecContext(saveCtx,
				`DELETE FROM idempotency_keys WHERE merchant_id = $1 AND key = $2`, merchantID, key)
		} else {
			_, err = m.DB.ExecContext(saveCtx, `UPDATE idempotency_keys
				SET status = 'completed', response_code = $3, response_body = $4, locked_at = NULL
				WHERE merchant_id = $1 AND key = $2`, merchantID, key, rw.status, rw.buf.Bytes())
		}
		if err != nil {
			// The response was already sent; a stuck key self-heals after LockTimeout.
			slog.Error("failed to persist idempotency result", "err", err, "key", key)
		}
	})
}

func (m Middleware) acquire(ctx context.Context, merchantID, key, hash string, lockTimeout time.Duration) (bool, *record, error) {
	for attempt := 0; attempt < 3; attempt++ {
		res, err := m.DB.ExecContext(ctx, `INSERT INTO idempotency_keys (merchant_id, key, request_hash, status, locked_at)
			VALUES ($1, $2, $3, 'in_progress', now()) ON CONFLICT DO NOTHING`, merchantID, key, hash)
		if err != nil {
			return false, nil, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			return true, nil, nil
		}

		// Take over a stale lock left by a crashed request with identical params.
		res, err = m.DB.ExecContext(ctx, `UPDATE idempotency_keys SET locked_at = now()
			WHERE merchant_id = $1 AND key = $2 AND request_hash = $3
			  AND status = 'in_progress' AND locked_at < now() - $4 * interval '1 second'`,
			merchantID, key, hash, lockTimeout.Seconds())
		if err != nil {
			return false, nil, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			return true, nil, nil
		}

		var rec record
		err = m.DB.QueryRowContext(ctx, `SELECT request_hash, status, response_code, response_body
			FROM idempotency_keys WHERE merchant_id = $1 AND key = $2`, merchantID, key).
			Scan(&rec.requestHash, &rec.status, &rec.responseCode, &rec.responseBody)
		if errors.Is(err, sql.ErrNoRows) {
			continue // released between our INSERT and SELECT; try again
		}
		if err != nil {
			return false, nil, err
		}
		return false, &rec, nil
	}
	return false, nil, errors.New("idempotency: could not acquire key after retries")
}

// Purge deletes keys older than maxAge (Stripe keeps them for 24h).
func Purge(ctx context.Context, db *sql.DB, maxAge time.Duration) (int64, error) {
	res, err := db.ExecContext(ctx, `DELETE FROM idempotency_keys WHERE created_at < now() - $1 * interval '1 second'`, maxAge.Seconds())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func requestHash(method, path string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(method + " " + path + "\n"))
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// recorder passes the response through while keeping a copy for replay.
type recorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	buf         bytes.Buffer
}

func (r *recorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status, r.wroteHeader = code, true
		r.ResponseWriter.WriteHeader(code)
	}
}

func (r *recorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	r.buf.Write(b)
	return r.ResponseWriter.Write(b)
}
