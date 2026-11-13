// Command minipayctl is an admin/dev CLI.
//
//	minipayctl create-merchant -name "Acme"
//	minipayctl simulate -payment pay_... -event succeeded [-url http://localhost:8080]
//
// `simulate` sends a correctly signed Stripe-style webhook for a payment, so the
// full flow can be demoed without a Stripe account (pair with MINIPAY_FAKE_STRIPE=1).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/sindhu/minipay/internal/auth"
	"github.com/sindhu/minipay/internal/ids"
	"github.com/sindhu/minipay/internal/payments"
	"github.com/sindhu/minipay/internal/store"
	"github.com/sindhu/minipay/internal/stripeapi"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	ctx := context.Background()
	var err error
	switch os.Args[1] {
	case "create-merchant":
		err = createMerchant(ctx, os.Args[2:])
	case "simulate":
		err = simulate(ctx, os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: minipayctl create-merchant -name NAME | simulate -payment ID -event succeeded|payment_failed|canceled|processing")
	os.Exit(2)
}

func createMerchant(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("create-merchant", flag.ExitOnError)
	name := fs.String("name", "Demo merchant", "merchant name")
	_ = fs.Parse(args)
	db, err := store.Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer db.Close()
	if err := store.Migrate(ctx, db); err != nil {
		return err
	}
	id, key, err := auth.CreateMerchant(ctx, db, *name)
	if err != nil {
		return err
	}
	fmt.Printf("merchant_id: %s\napi_key:     %s   (shown once; store it safely)\n", id, key)
	return nil
}

func simulate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("simulate", flag.ExitOnError)
	paymentID := fs.String("payment", "", "payment id (pay_...)")
	event := fs.String("event", "succeeded", "succeeded | payment_failed | canceled | processing")
	url := fs.String("url", "http://localhost:8080", "API base URL")
	secret := fs.String("secret", envOr("STRIPE_WEBHOOK_SECRET", "whsec_minipay_local_fake"), "webhook signing secret")
	_ = fs.Parse(args)
	if *paymentID == "" {
		return fmt.Errorf("-payment is required")
	}

	db, err := store.Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer db.Close()
	var amount int64
	var currency, merchantID string
	var piID *string
	if err := db.QueryRowContext(ctx, `SELECT amount, currency, merchant_id, stripe_pi_id FROM payments WHERE id = $1`,
		*paymentID).Scan(&amount, &currency, &merchantID, &piID); err != nil {
		return fmt.Errorf("load payment: %w", err)
	}
	if piID == nil {
		return fmt.Errorf("payment has no PaymentIntent yet")
	}

	pi := stripeapi.PaymentIntent{
		ID: *piID, Object: "payment_intent", Amount: amount, Currency: currency,
		Status:   map[string]string{"succeeded": "succeeded", "payment_failed": "requires_payment_method", "canceled": "canceled", "processing": "processing"}[*event],
		Metadata: map[string]string{payments.MetadataPaymentID: *paymentID, "merchant_id": merchantID},
	}
	if *event == "succeeded" {
		pi.AmountReceived = amount
	}
	obj, _ := json.Marshal(pi)
	body, _ := json.Marshal(map[string]any{
		"id": ids.New("evt_sim"), "object": "event", "type": "payment_intent." + *event,
		"created": time.Now().Unix(), "data": map[string]json.RawMessage{"object": obj},
	})

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, *url+"/internal/stripe/webhook", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Stripe-Signature", stripeapi.SignatureHeader(body, *secret, time.Now()))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	fmt.Printf("%d %s\n", resp.StatusCode, out)
	return nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
