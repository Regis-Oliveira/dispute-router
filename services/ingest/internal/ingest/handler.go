package ingest

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/regisoliveira/dispute-router/services/ingest/internal/signing"
)

const signatureHeader = "X-Processor-Signature"

type Handler struct {
	store           *Store
	guard           *Guard
	merchantLimiter *Limiter
	ipLimiter       *Limiter
	tolerance       time.Duration
	maxBodyBytes    int64
	logger          *slog.Logger
	now             func() time.Time
}

type HandlerOptions struct {
	Store           *Store
	Guard           *Guard
	MerchantLimiter *Limiter
	IPLimiter       *Limiter
	Tolerance       time.Duration
	MaxBodyBytes    int64
	Logger          *slog.Logger
}

func NewHandler(opts HandlerOptions) *Handler {
	return &Handler{
		store:           opts.Store,
		guard:           opts.Guard,
		merchantLimiter: opts.MerchantLimiter,
		ipLimiter:       opts.IPLimiter,
		tolerance:       opts.Tolerance,
		maxBodyBytes:    opts.MaxBodyBytes,
		logger:          opts.Logger,
		now:             time.Now,
	}
}

// ServeHTTP handles one webhook delivery.
//
// The order of the steps below is the security design, not an accident:
//
//  1. A per-IP limit and a body cap come first, because everything after them
//     costs a database round trip.
//  2. The body is parsed before the signature is checked - unavoidable, since
//     the merchant id that selects the secret is inside the body. Nothing from
//     that parse is acted on until step 4; it only chooses which key to verify
//     against.
//  3. An unknown merchant and a bad signature return the same 401. Different
//     answers would let anyone enumerate merchant ids.
//  4. Only after the signature holds does the request get to spend the
//     merchant's rate-limit budget or touch the write path.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logger := h.logger

	if allowed, _, err := h.ipLimiter.Allow(ctx, clientIP(r)); err != nil {
		logger.ErrorContext(ctx, "ip rate limit failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "rate limiter unavailable")
		return
	} else if !allowed {
		writeError(w, http.StatusTooManyRequests, "too many requests")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.maxBodyBytes))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "body too large")
		return
	}

	signatureValue := r.Header.Get(signatureHeader)
	if signatureValue == "" {
		writeError(w, http.StatusUnauthorized, "missing signature")
		return
	}

	event, err := Decode(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := event.Validate(h.now()); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	merchant, err := h.store.MerchantByExternalID(ctx, event.Data.MerchantID)
	if err != nil {
		if errors.Is(err, ErrUnknownMerchant) {
			// Same answer as a bad signature, on purpose.
			logger.WarnContext(ctx, "rejected delivery", "reason", "unknown merchant",
				"claimed_merchant", event.Data.MerchantID)
			writeError(w, http.StatusUnauthorized, "invalid signature")
			return
		}
		logger.ErrorContext(ctx, "merchant lookup failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	if err := signing.Verify(merchant.WebhookSecret, body, signatureValue, h.now(), h.tolerance); err != nil {
		logger.WarnContext(ctx, "rejected delivery", "reason", "signature", "error", err,
			"merchant", merchant.ExternalID)
		writeError(w, http.StatusUnauthorized, "invalid signature")
		return
	}

	// From here on the payload is trusted: it was signed with a secret only
	// this merchant and this platform hold.

	if allowed, remaining, err := h.merchantLimiter.Allow(ctx, merchant.ExternalID); err != nil {
		logger.ErrorContext(ctx, "merchant rate limit failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "rate limiter unavailable")
		return
	} else if !allowed {
		logger.WarnContext(ctx, "merchant rate limited", "merchant", merchant.ExternalID, "remaining", remaining)
		writeError(w, http.StatusTooManyRequests, "too many requests")
		return
	}

	// The idempotency key comes from the signed body, never from the
	// Idempotency-Key header. That header is not covered by the signature, so
	// keying off it would let anyone suppress a real event by guessing an id.
	claimed, err := h.guard.Claim(ctx, event.ID)
	if err != nil {
		logger.ErrorContext(ctx, "idempotency claim failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "idempotency store unavailable")
		return
	}
	if !claimed {
		writeJSON(w, http.StatusOK, map[string]any{"status": "duplicate", "event_id": event.ID})
		return
	}

	result, err := h.store.Record(ctx, merchant, body, signatureValue, event)
	if err != nil && !errors.Is(err, ErrUnknownTransaction) {
		// The claim is only valid if the work behind it committed. Hand it back
		// so the sender's retry is processed instead of being answered
		// "already handled" for the next day.
		if releaseErr := h.guard.Release(ctx, event.ID); releaseErr != nil {
			logger.ErrorContext(ctx, "could not release idempotency claim", "error", releaseErr, "event_id", event.ID)
		}
		logger.ErrorContext(ctx, "record failed", "error", err, "event_id", event.ID)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	if errors.Is(err, ErrUnknownTransaction) {
		// The delivery was stored; the dispute could not be linked. 422 tells
		// the sender this will not succeed on retry.
		logger.WarnContext(ctx, "unlinkable dispute", "merchant", merchant.ExternalID,
			"transaction", event.Data.TransactionID, "webhook_event_id", result.WebhookEventID)
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"status": "unlinkable", "reason": "unknown transaction", "event_id": event.ID,
		})
		return
	}

	if result.Duplicate {
		writeJSON(w, http.StatusOK, map[string]any{"status": "duplicate", "event_id": event.ID})
		return
	}

	logger.InfoContext(ctx, "dispute received",
		"merchant", merchant.ExternalID,
		"dispute_id", result.DisputeID,
		"kind", event.Data.Kind,
		"amount_minor", event.Data.AmountMinor,
		"currency", event.Data.Currency,
		"deadline_at", event.Data.RespondBy)

	writeJSON(w, http.StatusAccepted, map[string]any{
		"status":     "accepted",
		"event_id":   event.ID,
		"dispute_id": result.DisputeID,
	})
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": message})
}
