// Package api wires HTTP routes and middleware.
package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/sindhu/minipay/internal/apierr"
	"github.com/sindhu/minipay/internal/auth"
	"github.com/sindhu/minipay/internal/idempotency"
	"github.com/sindhu/minipay/internal/ids"
	"github.com/sindhu/minipay/internal/payments"
	"github.com/sindhu/minipay/internal/ratelimit"
	"github.com/sindhu/minipay/internal/stripeapi"
)

type Server struct {
	DB            *sql.DB
	Payments      *payments.Service
	Limiter       *ratelimit.Limiter
	WebhookSecret string
	Log           *slog.Logger
	Now           func() time.Time
}

func (s *Server) Handler() http.Handler {
	if s.Now == nil {
		s.Now = time.Now
	}
	v1 := http.NewServeMux()
	v1.HandleFunc("POST /v1/payments", s.createPayment)
	v1.HandleFunc("GET /v1/payments/{id}", s.getPayment)
	v1.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
		apierr.Write(w, apierr.NotFound("unrecognized request URL "+r.Method+" "+r.URL.Path))
	})

	idem := idempotency.Middleware{DB: s.DB}
	authed := auth.Middleware(s.DB)(s.Limiter.Middleware(idem.Wrap(v1)))

	mux := http.NewServeMux()
	mux.Handle("/v1/", authed)
	mux.HandleFunc("POST /internal/stripe/webhook", s.stripeWebhook)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.DB.PingContext(r.Context()); err != nil {
			apierr.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "db_unavailable"})
			return
		}
		apierr.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	return s.logRequests(recoverPanics(mux))
}

func (s *Server) createPayment(w http.ResponseWriter, r *http.Request) {
	var p payments.CreateParams
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		apierr.Write(w, apierr.InvalidRequest("body_invalid", "", "invalid JSON body: "+err.Error()))
		return
	}
	pay, err := s.Payments.Create(r.Context(), auth.MerchantID(r.Context()), idempotency.KeyFromContext(r.Context()), p)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, pay)
}

func (s *Server) getPayment(w http.ResponseWriter, r *http.Request) {
	pay, err := s.Payments.Get(r.Context(), auth.MerchantID(r.Context()), r.PathValue("id"))
	if err != nil {
		apierr.Write(w, err)
		return
	}
	pay.ClientSecret = nil // only returned at creation
	apierr.WriteJSON(w, http.StatusOK, pay)
}

// stripeWebhook receives events from Stripe. Responses: 2xx = processed or
// safely ignored; 400 = bad signature (Stripe will not fix it by retrying, but
// will retry anyway); 500 = transient failure, Stripe retries with backoff.
func (s *Server) stripeWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 256<<10))
	if err != nil {
		apierr.Write(w, apierr.InvalidRequest("body_unreadable", "", "could not read body"))
		return
	}
	if err := stripeapi.VerifySignature(body, r.Header.Get("Stripe-Signature"), s.WebhookSecret, 5*time.Minute, s.Now()); err != nil {
		s.Log.Warn("rejected stripe webhook", "err", err)
		apierr.Write(w, apierr.InvalidRequest("signature_invalid", "Stripe-Signature", err.Error()))
		return
	}
	var ev stripeapi.Event
	if err := json.Unmarshal(body, &ev); err != nil || ev.ID == "" {
		apierr.Write(w, apierr.InvalidRequest("body_invalid", "", "malformed event"))
		return
	}
	result, err := s.Payments.HandleStripeEvent(r.Context(), ev)
	if err != nil {
		s.Log.Error("failed to apply stripe event", "event", ev.ID, "type", ev.Type, "err", err)
		apierr.Write(w, errors.New("event processing failed"))
		return
	}
	s.Log.Info("stripe event", "event", ev.ID, "type", ev.Type, "result", result)
	apierr.WriteJSON(w, http.StatusOK, map[string]string{"received": "true", "result": result})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := ids.New("req")
		w.Header().Set("Request-Id", reqID)
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(sw, r)
		s.Log.Info("http", "request_id", reqID, "method", r.Method, "path", r.URL.Path,
			"status", sw.status, "duration_ms", time.Since(start).Milliseconds())
	})
}

func recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				slog.Error("panic", "value", v, "path", r.URL.Path)
				apierr.Write(w, errors.New("panic"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}
