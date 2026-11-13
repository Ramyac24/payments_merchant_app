package stripeapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

var (
	ErrInvalidSignature = errors.New("stripe webhook: invalid signature")
	ErrTimestamp        = errors.New("stripe webhook: timestamp outside tolerance")
)

// Event is an incoming Stripe webhook event.
type Event struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Created int64  `json:"created"`
	Data    struct {
		Object json.RawMessage `json:"object"`
	} `json:"data"`
}

// VerifySignature checks a Stripe-Signature header ("t=<unix>,v1=<hex>[,v1=...]").
// The signed payload is "<t>.<raw body>" HMAC-SHA256'd with the endpoint secret.
// The timestamp tolerance defends against replay of captured requests.
func VerifySignature(payload []byte, header, secret string, tolerance time.Duration, now time.Time) error {
	var ts string
	var sigs []string
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			ts = v
		case "v1":
			sigs = append(sigs, v)
		}
	}
	if ts == "" || len(sigs) == 0 {
		return ErrInvalidSignature
	}
	unix, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return ErrInvalidSignature
	}
	if d := now.Sub(time.Unix(unix, 0)); d > tolerance || d < -tolerance {
		return ErrTimestamp
	}
	expected := computeSignature(ts, payload, secret)
	for _, s := range sigs {
		got, err := hex.DecodeString(s)
		if err == nil && hmac.Equal(got, expected) { // constant-time compare
			return nil
		}
	}
	return ErrInvalidSignature
}

// SignatureHeader builds a valid Stripe-Signature header (used by tests and the CLI simulator).
func SignatureHeader(payload []byte, secret string, t time.Time) string {
	ts := strconv.FormatInt(t.Unix(), 10)
	return fmt.Sprintf("t=%s,v1=%s", ts, hex.EncodeToString(computeSignature(ts, payload, secret)))
}

func computeSignature(ts string, payload []byte, secret string) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(payload)
	return mac.Sum(nil)
}
