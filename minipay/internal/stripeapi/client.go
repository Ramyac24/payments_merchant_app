// Package stripeapi is a small, dependency-free client for the parts of the
// Stripe REST API this project uses, plus webhook signature verification.
package stripeapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type PaymentIntent struct {
	ID             string            `json:"id"`
	Object         string            `json:"object"`
	Amount         int64             `json:"amount"`
	AmountReceived int64             `json:"amount_received"`
	Currency       string            `json:"currency"`
	Status         string            `json:"status"`
	ClientSecret   string            `json:"client_secret"`
	Metadata       map[string]string `json:"metadata"`
}

type CreatePaymentIntentParams struct {
	Amount         int64
	Currency       string
	Description    string
	Metadata       map[string]string
	IdempotencyKey string // forwarded to Stripe; makes retries safe end to end
}

// Provider is what the payments service needs from a payment processor.
type Provider interface {
	CreatePaymentIntent(ctx context.Context, p CreatePaymentIntentParams) (*PaymentIntent, error)
}

// Error is an error response returned by Stripe.
type Error struct {
	HTTPStatus int    `json:"-"`
	Type       string `json:"type"`
	Code       string `json:"code"`
	Message    string `json:"message"`
	Param      string `json:"param"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("stripe %d %s: %s", e.HTTPStatus, e.Type, e.Message)
}

// Permanent reports whether retrying the same request can never succeed
// (bad params, card declined, auth error) as opposed to a transient failure.
func (e *Error) Permanent() bool {
	return e.HTTPStatus >= 400 && e.HTTPStatus < 500 && e.HTTPStatus != http.StatusTooManyRequests &&
		e.HTTPStatus != http.StatusConflict
}

type Client struct {
	SecretKey  string
	BaseURL    string
	HTTP       *http.Client
	MaxRetries int
}

func NewClient(secretKey string) *Client {
	return &Client{
		SecretKey:  secretKey,
		BaseURL:    "https://api.stripe.com",
		HTTP:       &http.Client{Timeout: 30 * time.Second},
		MaxRetries: 2,
	}
}

func (c *Client) CreatePaymentIntent(ctx context.Context, p CreatePaymentIntentParams) (*PaymentIntent, error) {
	form := url.Values{}
	form.Set("amount", strconv.FormatInt(p.Amount, 10))
	form.Set("currency", p.Currency)
	form.Set("automatic_payment_methods[enabled]", "true")
	if p.Description != "" {
		form.Set("description", p.Description)
	}
	for k, v := range p.Metadata {
		form.Set("metadata["+k+"]", v)
	}
	var pi PaymentIntent
	if err := c.post(ctx, "/v1/payment_intents", form, p.IdempotencyKey, &pi); err != nil {
		return nil, err
	}
	return &pi, nil
}

// post sends a form-encoded POST. Because every call carries an Idempotency-Key,
// transient failures (network errors, 429, 5xx) are retried with backoff.
func (c *Client) post(ctx context.Context, path string, form url.Values, idemKey string, out any) error {
	var lastErr error
	for attempt := 0; attempt <= c.MaxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<(attempt-1)) * 500 * time.Millisecond
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		lastErr = c.do(ctx, path, form, idemKey, out)
		if lastErr == nil {
			return nil
		}
		var se *Error
		if errors.As(lastErr, &se) && se.Permanent() {
			return lastErr
		}
		if ctx.Err() != nil {
			return lastErr
		}
	}
	return lastErr
}

func (c *Client) do(ctx context.Context, path string, form url.Values, idemKey string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.SecretKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) {
			return fmt.Errorf("stripe network error: %w", err)
		}
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var wrapper struct {
			Error Error `json:"error"`
		}
		_ = json.Unmarshal(body, &wrapper)
		wrapper.Error.HTTPStatus = resp.StatusCode
		if wrapper.Error.Message == "" {
			wrapper.Error.Message = http.StatusText(resp.StatusCode)
		}
		return &wrapper.Error
	}
	return json.Unmarshal(body, out)
}
