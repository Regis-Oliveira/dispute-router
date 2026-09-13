package ingest

import (
	"context"
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
//
// Each step stays here, in that order. What moved out is the part that differs
// between the two event shapes, behind the delivery interface below: this used
// to carry a half-populated DisputeWebhook and a half-populated RulingWebhook
// side by side from the first switch to the last, through every step that cared
// about neither.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logger := h.logger

	// 1.
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

	// 2.
	event, err := decodeDelivery(body, h.now())
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	// 3.
	merchant, ok := h.authenticate(ctx, w, body, signatureValue, event.merchantID())
	if !ok {
		return
	}

	// From here on the payload is trusted: it was signed with a secret only
	// this merchant and this platform hold.

	// 4.
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
	eventID := event.eventID()
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

	h.record(ctx, w, merchant, body, signatureValue, event)
}

// authenticate answers "which merchant signed this, if any".
//
// It is the whole of step 3: an unknown merchant and a bad signature leave by
// the same door with the same 401 body, because two different answers would
// turn the endpoint into a merchant-id oracle. Everything it needs is the
// merchant id claimed by the unverified body, which is why this runs before
// anything is acted on and after nothing is.
func (h *Handler) authenticate(
	ctx context.Context, w http.ResponseWriter,
	body []byte, signatureValue, claimedMerchant string,
) (Merchant, bool) {
	logger := h.logger

	merchant, err := h.store.MerchantByExternalID(ctx, claimedMerchant)
	if err != nil {
		if errors.Is(err, ErrUnknownMerchant) {
			// Same answer as a bad signature, on purpose.
			logger.WarnContext(ctx, "rejected delivery", "reason", "unknown merchant",
				"claimed_merchant", claimedMerchant)
			httpx.WriteError(w, http.StatusUnauthorized, "invalid signature")
			return Merchant{}, false
		}
		logger.ErrorContext(ctx, "merchant lookup failed", "error", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return Merchant{}, false
	}

	// Resolved separately from the merchant row, and never logged. The set is
	// plural so a key can be rotated without a cutover that rejects every
	// in-flight delivery.
	merchantSecrets, err := h.secrets.SecretsFor(ctx, merchant.ExternalID)
	if err != nil {
		logger.ErrorContext(ctx, "secret lookup failed", "error", err, "merchant", merchant.ExternalID)
		httpx.WriteError(w, http.StatusServiceUnavailable, "cannot verify signatures right now")
		return Merchant{}, false
	}

	if err := signing.VerifyAny(merchantSecrets, body, signatureValue, h.now(), h.tolerance); err != nil {
		logger.WarnContext(ctx, "rejected delivery", "reason", "signature", "error", err,
			"merchant", merchant.ExternalID)
		httpx.WriteError(w, http.StatusUnauthorized, "invalid signature")
		return Merchant{}, false
	}

	return merchant, true
}

// record runs the write path and answers with what it did.
//
// Reached only with the idempotency claim held, so every path out of it either
// commits the work the claim stands for or hands the claim back.
func (h *Handler) record(
	ctx context.Context, w http.ResponseWriter,
	merchant Merchant, body []byte, signatureValue string, event delivery,
) {
	logger := h.logger
	eventID := event.eventID()

	result, recordErr := event.record(ctx, h.store, merchant, body, signatureValue)

	if recordErr != nil && !errors.Is(recordErr, event.unlinkable()) {
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

	event.logAccepted(ctx, logger, merchant, result)

	httpx.WriteJSON(w, http.StatusAccepted, map[string]any{
		"status":     "accepted",
		"event_id":   eventID,
		"dispute_id": result.DisputeID,
	})
}

// delivery is one decoded webhook, whichever shape arrived.
//
// Small on purpose: the pipeline above needs the idempotency key and the
// claimed merchant id before it trusts anything, and after it does, one call
// that writes and one line that says so.
type delivery interface {
	// eventID is the idempotency key, taken from the signed body.
	eventID() string

	// merchantID is who the body claims to be from - unverified, and used only
	// to choose which key to verify against.
	merchantID() string

	// record writes the delivery through the store.
	record(ctx context.Context, store *Store, merchant Merchant, raw []byte, signature string) (recordResult, error)

	// unlinkable is the one error from record that means the delivery was
	// stored but has nothing to act on, which is a 422 rather than a 500.
	unlinkable() error

	// logAccepted writes the line that says what was recorded.
	logAccepted(ctx context.Context, logger *slog.Logger, merchant Merchant, result recordResult)
}

// decodeDelivery parses a body into whichever shape its type names.
//
// The type decides which decoder runs. Each one is strict about its own shape,
// so a ruling body cannot be quietly read as a dispute.
func decodeDelivery(body []byte, now time.Time) (delivery, error) {
	_, eventType, err := peekType(body)
	if err != nil {
		return nil, err
	}

	switch eventType {
	case TypeDisputeOpened:
		opened, err := decodeDispute(body)
		if err != nil {
			return nil, err
		}
		if err := opened.Validate(now); err != nil {
			return nil, err
		}
		return opened, nil

	case TypeDisputeResolved:
		ruling, err := decodeRuling(body)
		if err != nil {
			return nil, err
		}
		if err := ruling.Validate(); err != nil {
			return nil, err
		}
		return ruling, nil

	default:
		// Deliberately not wrapped in ErrInvalidEvent: the reason the sender
		// reads is the type it sent, not a prefix about validity.
		return nil, errors.New("unsupported event type " + eventType)
	}
}

func (e DisputeWebhook) eventID() string    { return e.ID }
func (e DisputeWebhook) merchantID() string { return e.Data.MerchantID }

func (e DisputeWebhook) record(
	ctx context.Context, store *Store, merchant Merchant, raw []byte, signature string,
) (recordResult, error) {
	return store.record(ctx, merchant, raw, signature, e)
}

func (e DisputeWebhook) unlinkable() error { return ErrUnknownTransaction }

func (e DisputeWebhook) logAccepted(
	ctx context.Context, logger *slog.Logger, merchant Merchant, result recordResult,
) {
	logger.InfoContext(ctx, "dispute received",
		"merchant", merchant.ExternalID,
		"dispute_id", result.DisputeID,
		"kind", e.Data.Kind,
		"amount_minor", e.Data.AmountMinor,
		"currency", e.Data.Currency,
		"deadline_at", e.Data.RespondBy)
}

func (e RulingWebhook) eventID() string    { return e.ID }
func (e RulingWebhook) merchantID() string { return e.Data.MerchantID }

func (e RulingWebhook) record(
	ctx context.Context, store *Store, merchant Merchant, raw []byte, signature string,
) (recordResult, error) {
	return store.recordRuling(ctx, merchant, raw, signature, e)
}

func (e RulingWebhook) unlinkable() error { return ErrNotRepresented }

func (e RulingWebhook) logAccepted(
	ctx context.Context, logger *slog.Logger, merchant Merchant, result recordResult,
) {
	logger.InfoContext(ctx, "network ruled",
		"merchant", merchant.ExternalID,
		"dispute_id", result.DisputeID,
		"outcome", e.Data.Outcome)
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
