package ingest

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/regisoliveira/dispute-router/internal/httpx"
	"github.com/regisoliveira/dispute-router/internal/secrets"
	"github.com/regisoliveira/dispute-router/internal/signing"
)

const signatureHeader = "X-Processor-Signature"

// Handler is the webhook endpoint: one delivery in, one status out.
type Handler struct {
	store           *Store
	secrets         secrets.Resolver
	guard           *Guard
	merchantLimiter *Limiter
	ipLimiter       *Limiter
	tolerance       time.Duration
	maxBodyBytes    int64
	logger          *slog.Logger
	now             func() time.Time
}

// HandlerOptions is everything a Handler needs; the clock is the only optional part.
type HandlerOptions struct {
	Store           *Store
	Secrets         secrets.Resolver
	Guard           *Guard
	MerchantLimiter *Limiter
	IPLimiter       *Limiter
	Tolerance       time.Duration
	MaxBodyBytes    int64
	Logger          *slog.Logger

	// Now is the clock the timestamp checks read. Nil means time.Now; a test
	// sets it to pin what "too old" and "in the future" mean.
	Now func() time.Time
}

// NewHandler builds a Handler from its options.
func NewHandler(opts HandlerOptions) *Handler {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Handler{
		store:           opts.Store,
		secrets:         opts.Secrets,
		guard:           opts.Guard,
		merchantLimiter: opts.MerchantLimiter,
		ipLimiter:       opts.IPLimiter,
		tolerance:       opts.Tolerance,
		maxBodyBytes:    opts.MaxBodyBytes,
		logger:          opts.Logger,
		now:             now,
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
		httpx.WriteError(w, http.StatusServiceUnavailable, "rate limiter unavailable")
		return
	} else if !allowed {
		httpx.WriteError(w, http.StatusTooManyRequests, "too many requests")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.maxBodyBytes))
	if err != nil {
		// Only the cap is 413. A client that hangs up mid-body used to be told
		// its body was too large, which is a lie about a limit it never hit.
		if errors.As(err, new(*http.MaxBytesError)) {
			httpx.WriteError(w, http.StatusRequestEntityTooLarge, "body too large")
			return
		}
		httpx.WriteError(w, http.StatusBadRequest, "could not read body")
		return
	}

	signatureValue := r.Header.Get(signatureHeader)
	if signatureValue == "" {
		httpx.WriteError(w, http.StatusUnauthorized, "missing signature")
		return
	}

	// The type decides which decoder runs. Each one is strict about its own
	// shape, so a ruling body cannot be quietly read as a dispute.
	_, eventType, err := peekType(body)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	var (
		opened     DisputeWebhook
		ruling     RulingWebhook
		eventID    string
		merchantID string
	)

	switch eventType {
	case TypeDisputeOpened:
		opened, err = decodeDispute(body)
		if err == nil {
			err = opened.Validate(h.now())
		}
		eventID, merchantID = opened.ID, opened.Data.MerchantID

	case TypeDisputeResolved:
		ruling, err = decodeRuling(body)
		if err == nil {
			err = ruling.Validate()
		}
		eventID, merchantID = ruling.ID, ruling.Data.MerchantID

	default:
		httpx.WriteError(w, http.StatusBadRequest, "unsupported event type "+eventType)
		return
	}

	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	merchant, err := h.store.MerchantByExternalID(ctx, merchantID)
	if err != nil {
		if errors.Is(err, ErrUnknownMerchant) {
			// Same answer as a bad signature, on purpose.
			logger.WarnContext(ctx, "rejected delivery", "reason", "unknown merchant",
				"claimed_merchant", merchantID)
			httpx.WriteError(w, http.StatusUnauthorized, "invalid signature")
			return
		}
		logger.ErrorContext(ctx, "merchant lookup failed", "error", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Resolved separately from the merchant row, and never logged. The set is
	// plural so a key can be rotated without a cutover that rejects every
	// in-flight delivery.
	merchantSecrets, err := h.secrets.SecretsFor(ctx, merchant.ExternalID)
	if err != nil {
		logger.ErrorContext(ctx, "secret lookup failed", "error", err, "merchant", merchant.ExternalID)
		httpx.WriteError(w, http.StatusServiceUnavailable, "cannot verify signatures right now")
		return
	}

	if err := signing.VerifyAny(merchantSecrets, body, signatureValue, h.now(), h.tolerance); err != nil {
		logger.WarnContext(ctx, "rejected delivery", "reason", "signature", "error", err,
			"merchant", merchant.ExternalID)
		httpx.WriteError(w, http.StatusUnauthorized, "invalid signature")
		return
	}

	// From here on the payload is trusted: it was signed with a secret only
	// this merchant and this platform hold.

	if allowed, remaining, err := h.merchantLimiter.Allow(ctx, merchant.ExternalID); err != nil {
		logger.ErrorContext(ctx, "merchant rate limit failed", "error", err)
		httpx.WriteError(w, http.StatusServiceUnavailable, "rate limiter unavailable")
		return
	} else if !allowed {
		logger.WarnContext(ctx, "merchant rate limited", "merchant", merchant.ExternalID, "remaining", remaining)
		httpx.WriteError(w, http.StatusTooManyRequests, "too many requests")
		return
	}

	// The idempotency key comes from the signed body, never from the
	// Idempotency-Key header. That header is not covered by the signature, so
	// keying off it would let anyone suppress a real event by guessing an id.
	claimed, err := h.guard.Claim(ctx, eventID)
	if err != nil {
		logger.ErrorContext(ctx, "idempotency claim failed", "error", err)
		httpx.WriteError(w, http.StatusServiceUnavailable, "idempotency store unavailable")
		return
	}
	if !claimed {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": "duplicate", "event_id": eventID})
		return
	}

	var (
		result     recordResult
		recordErr  error
		unlinkable error
	)

	switch eventType {
	case TypeDisputeOpened:
		result, recordErr = h.store.record(ctx, merchant, body, signatureValue, opened)
		unlinkable = ErrUnknownTransaction
	case TypeDisputeResolved:
		result, recordErr = h.store.recordRuling(ctx, merchant, body, signatureValue, ruling)
		unlinkable = ErrNotRepresented
	}

	if recordErr != nil && !errors.Is(recordErr, unlinkable) {
		// The claim is only valid if the work behind it committed. Hand it back
		// so the sender's retry is processed instead of being answered
		// "already handled" for the next day.
		if releaseErr := h.guard.Release(ctx, eventID); releaseErr != nil {
			logger.ErrorContext(ctx, "could not release idempotency claim", "error", releaseErr, "event_id", eventID)
		}
		logger.ErrorContext(ctx, "record failed", "error", recordErr, "event_id", eventID)
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}

	if recordErr != nil {
		// The delivery was stored; it could not be linked to anything to act
		// on. 422 tells the sender this will not succeed on retry.
		logger.WarnContext(ctx, "unlinkable delivery", "merchant", merchant.ExternalID,
			"reason", recordErr.Error(), "webhook_event_id", result.WebhookEventID)
		httpx.WriteJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"status": "unlinkable", "reason": recordErr.Error(), "event_id": eventID,
		})
		return
	}

	if result.Duplicate {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": "duplicate", "event_id": eventID})
		return
	}

	switch eventType {
	case TypeDisputeOpened:
		logger.InfoContext(ctx, "dispute received",
			"merchant", merchant.ExternalID,
			"dispute_id", result.DisputeID,
			"kind", opened.Data.Kind,
			"amount_minor", opened.Data.AmountMinor,
			"currency", opened.Data.Currency,
			"deadline_at", opened.Data.RespondBy)
	case TypeDisputeResolved:
		logger.InfoContext(ctx, "network ruled",
			"merchant", merchant.ExternalID,
			"dispute_id", result.DisputeID,
			"outcome", ruling.Data.Outcome)
	}

	httpx.WriteJSON(w, http.StatusAccepted, map[string]any{
		"status":     "accepted",
		"event_id":   eventID,
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
