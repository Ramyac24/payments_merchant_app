// Package auth authenticates merchants by API key ("Authorization: Bearer mp_test_...").
package auth

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"github.com/sindhu/minipay/internal/apierr"
	"github.com/sindhu/minipay/internal/ids"
	"github.com/sindhu/minipay/internal/store"
)

type ctxKey struct{}

// MerchantID returns the authenticated merchant for this request ("" if none).
func MerchantID(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}

func WithMerchant(ctx context.Context, merchantID string) context.Context {
	return context.WithValue(ctx, ctxKey{}, merchantID)
}

// HashKey is how API keys are stored: sha256, hex. Keys are high-entropy random
// strings, so a fast hash is fine here (unlike user passwords).
func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func Middleware(db *sql.DB) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || key == "" {
				apierr.Write(w, apierr.Unauthorized("missing API key: use 'Authorization: Bearer <key>'"))
				return
			}
			var merchantID string
			err := db.QueryRowContext(r.Context(),
				`SELECT merchant_id FROM api_keys WHERE key_hash = $1`, HashKey(key)).Scan(&merchantID)
			if errors.Is(err, sql.ErrNoRows) {
				apierr.Write(w, apierr.Unauthorized("invalid API key"))
				return
			}
			if err != nil {
				apierr.Write(w, err)
				return
			}
			next.ServeHTTP(w, r.WithContext(WithMerchant(r.Context(), merchantID)))
		})
	}
}

// CreateMerchant creates a merchant and returns its id and a new plaintext API key.
// The plaintext key is shown once; only its hash is stored.
func CreateMerchant(ctx context.Context, db *sql.DB, name string) (merchantID, apiKey string, err error) {
	merchantID = ids.New("mch")
	apiKey = "mp_test_" + ids.Random(24)
	err = store.WithTx(ctx, db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO merchants (id, name) VALUES ($1, $2)`, merchantID, name); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO api_keys (key_hash, merchant_id) VALUES ($1, $2)`, HashKey(apiKey), merchantID)
		return err
	})
	return merchantID, apiKey, err
}
