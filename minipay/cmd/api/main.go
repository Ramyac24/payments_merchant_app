// Command api runs the minipay HTTP API.
//
// Environment:
//
//	DATABASE_URL           postgres://... (required)
//	ADDR                   listen address (default :8080)
//	STRIPE_SECRET_KEY      sk_test_... (live keys are refused)
//	STRIPE_WEBHOOK_SECRET  whsec_... (from the dashboard or `stripe listen`)
//	MINIPAY_FAKE_STRIPE=1  use an in-memory fake Stripe instead (no account needed)
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/sindhu/minipay/internal/api"
	"github.com/sindhu/minipay/internal/idempotency"
	"github.com/sindhu/minipay/internal/payments"
	"github.com/sindhu/minipay/internal/ratelimit"
	"github.com/sindhu/minipay/internal/store"
	"github.com/sindhu/minipay/internal/stripeapi"
)

const fakeWebhookSecret = "whsec_minipay_local_fake"

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(log)
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}
	db, err := store.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := store.Migrate(ctx, db); err != nil {
		return err
	}

	var provider stripeapi.Provider
	webhookSecret := os.Getenv("STRIPE_WEBHOOK_SECRET")
	switch key := os.Getenv("STRIPE_SECRET_KEY"); {
	case os.Getenv("MINIPAY_FAKE_STRIPE") == "1":
		provider = stripeapi.NewFake()
		if webhookSecret == "" {
			webhookSecret = fakeWebhookSecret
		}
		log.Warn("using FAKE Stripe provider; use `minipayctl simulate` to send webhooks")
	case strings.HasPrefix(key, "sk_live_"), strings.HasPrefix(key, "rk_live_"):
		return errors.New("refusing to start with a live Stripe key; this project is test-mode only")
	case key == "":
		return errors.New("set STRIPE_SECRET_KEY (sk_test_...) or MINIPAY_FAKE_STRIPE=1")
	default:
		provider = stripeapi.NewClient(key)
		if webhookSecret == "" {
			return errors.New("STRIPE_WEBHOOK_SECRET is required with a real Stripe key")
		}
	}

	srv := &api.Server{
		DB:            db,
		Payments:      &payments.Service{DB: db, Stripe: provider, Log: log},
		Limiter:       ratelimit.New(25, 50),
		WebhookSecret: webhookSecret,
		Log:           log,
	}
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      45 * time.Second,
	}

	go func() { // expire idempotency keys after 24h
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if n, err := idempotency.Purge(ctx, db, 24*time.Hour); err != nil {
					log.Error("purge idempotency keys", "err", err)
				} else if n > 0 {
					log.Info("purged idempotency keys", "count", n)
				}
			}
		}
	}()

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", addr)
		errCh <- httpSrv.ListenAndServe()
	}()
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
}
