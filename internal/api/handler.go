package api

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

type Handler struct {
	store    *Store
	rdb      *redis.Client
	evidence *Evidence
	logger   *slog.Logger
}

func NewHandler(store *Store, rdb *redis.Client, evidence *Evidence, logger *slog.Logger) *Handler {
	return &Handler{store: store, rdb: rdb, evidence: evidence, logger: logger}
}

// Routes returns the read model. Every path is a GET: this service never
// writes, which is why it can be scaled and cached independently of ingest.
func (h *Handler) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/disputes", h.listDisputes)
	mux.HandleFunc("GET /api/disputes.csv", h.exportDisputes)
	mux.HandleFunc("GET /api/disputes/{id}", h.getDispute)
	mux.HandleFunc("GET /api/disputes/{id}/evidence", h.listEvidence)
	// POST because it is not safe to cache, though it writes nothing: minting
	// a presigned URL reads a credential, it does not change any state. The
	// service stays read-only with respect to Postgres.
	mux.HandleFunc("POST /api/disputes/{id}/evidence", h.presignEvidence)
	mux.HandleFunc("GET /api/summary", h.summary)
	mux.HandleFunc("GET /api/merchants", h.merchants)
	mux.HandleFunc("GET /api/stream", h.stream)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", h.ready)
	return mux
}

func (h *Handler) listDisputes(w http.ResponseWriter, r *http.Request) {
	filters, err := ParseFilters(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	list, err := h.store.ListDisputes(r.Context(), filters)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "list disputes failed", "error", err)
		writeError(w, http.StatusInternalServerError, "query failed")
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (h *Handler) getDispute(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "id must be an integer")
		return
	}

	detail, err := h.store.Dispute(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such dispute")
		return
	}
	if err != nil {
		h.logger.ErrorContext(r.Context(), "load dispute failed", "error", err, "id", id)
		writeError(w, http.StatusInternalServerError, "query failed")
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func (h *Handler) listEvidence(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "id must be an integer")
		return
	}

	files, err := h.evidence.List(r.Context(), id)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "list evidence failed", "error", err, "dispute_id", id)
		writeError(w, http.StatusInternalServerError, "could not list evidence")
		return
	}
	writeJSON(w, http.StatusOK, files)
}

func (h *Handler) presignEvidence(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "id must be an integer")
		return
	}

	var body struct {
		Filename    string `json:"filename"`
		ContentType string `json:"content_type"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "body must be {filename, content_type}")
		return
	}

	// The dispute has to exist. Without this the endpoint mints upload URLs for
	// any integer anybody asks about, which is a way to write into the bucket
	// under keys that will never be read.
	if _, err := h.store.Dispute(r.Context(), id); err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, http.StatusNotFound, "no such dispute")
			return
		}
		h.logger.ErrorContext(r.Context(), "dispute lookup failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	target, err := h.evidence.PresignUpload(r.Context(), id, body.Filename, body.ContentType)
	if err != nil {
		if errors.Is(err, ErrInvalidInput) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		h.logger.ErrorContext(r.Context(), "presign failed", "error", err, "dispute_id", id)
		writeError(w, http.StatusInternalServerError, "could not create an upload url")
		return
	}
	writeJSON(w, http.StatusOK, target)
}

func (h *Handler) summary(w http.ResponseWriter, r *http.Request) {
	filters, err := ParseFilters(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	summary, err := h.store.Summary(r.Context(), filters)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "summary failed", "error", err)
		writeError(w, http.StatusInternalServerError, "query failed")
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

func (h *Handler) merchants(w http.ResponseWriter, r *http.Request) {
	options, err := h.store.Merchants(r.Context())
	if err != nil {
		h.logger.ErrorContext(r.Context(), "merchants failed", "error", err)
		writeError(w, http.StatusInternalServerError, "query failed")
		return
	}
	writeJSON(w, http.StatusOK, options)
}

// exportDisputes streams a CSV of everything matching the filters.
//
// The status line and headers go out before the first row is read, so the
// browser starts the download immediately. That is also the trade: once 200 is
// sent, a mid-stream database error cannot become a 500 - the file just ends.
// The row count in the trailer is how the client can tell it got everything.
func (h *Handler) exportDisputes(w http.ResponseWriter, r *http.Request) {
	filters, err := ParseFilters(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// The export is not paged; the whole filtered set is the point.
	filters.Offset = 0

	filename := fmt.Sprintf("disputes-%s.csv", time.Now().UTC().Format("20060102-150405"))
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")

	writer := csv.NewWriter(w)
	count, err := h.store.StreamCSV(r.Context(), filters, writer)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "csv export failed mid-stream",
			"error", err, "rows_written", count)
		return
	}
	h.logger.InfoContext(r.Context(), "csv export", "rows", count)
}

// stream is the live feed.
//
// The ingest service's outbox relay publishes to a Redis channel; this
// subscribes and forwards over Server-Sent Events. SSE rather than WebSockets
// because the data only travels one way - the browser has nothing to say back,
// and SSE reconnects on its own.
func (h *Handler) stream(w http.ResponseWriter, r *http.Request) {
	// ResponseController rather than a w.(http.Flusher) assertion: middleware
	// wraps the writer, and an assertion only sees the outermost layer.
	rc := http.NewResponseController(w)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	ctx := r.Context()
	sub := h.rdb.Subscribe(ctx, LiveChannel)
	defer func() { _ = sub.Close() }()

	messages := sub.Channel()
	// Proxies drop a connection that goes quiet. A comment line every 20s is
	// invisible to EventSource and keeps the socket alive through them.
	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()

	// Tells EventSource how long to wait before reconnecting.
	fmt.Fprint(w, "retry: 3000\n\n")
	if err := rc.Flush(); err != nil {
		h.logger.ErrorContext(ctx, "sse flush failed", "error", err)
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": keep-alive\n\n")
			if err := rc.Flush(); err != nil {
				return
			}
		case msg, open := <-messages:
			if !open {
				return
			}
			// The payload is already JSON from the relay; forwarding it
			// verbatim keeps this handler out of the schema's business.
			fmt.Fprintf(w, "event: dispute\ndata: %s\n\n", msg.Payload)
			if err := rc.Flush(); err != nil {
				return
			}
		}
	}
}

func (h *Handler) ready(w http.ResponseWriter, r *http.Request) {
	checks := map[string]string{"postgres": "ok", "redis": "ok"}
	status := http.StatusOK

	if err := h.store.Ping(r.Context()); err != nil {
		checks["postgres"] = err.Error()
		status = http.StatusServiceUnavailable
	}
	if err := h.rdb.Ping(r.Context()).Err(); err != nil {
		checks["redis"] = err.Error()
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, checks)
}

// CORS allows exactly the origins passed in.
//
// A reflected origin or a bare "*" would let any page on the internet read this
// data from a logged-in operator's browser. The dev server's origin is
// configuration, not a default.
func CORS(allowed []string) func(http.Handler) http.Handler {
	permitted := make(map[string]bool, len(allowed))
	for _, origin := range allowed {
		permitted[origin] = true
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin != "" && permitted[origin] {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				// Tells caches that the response body depends on the Origin, so
				// one origin's response is never served to another.
				w.Header().Add("Vary", "Origin")
				// POST is here for the presigned-upload endpoint. It mints a
				// credential rather than changing state, but the browser does
				// not know that: a preflight for a method not listed here is
				// refused before the request is ever sent.
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
				w.Header().Set("Access-Control-Max-Age", "600")
			}

			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// LiveChannel is the Redis pub/sub channel the outbox relay publishes to.
const LiveChannel = "disputes:live"

// Timeout fails a request that outruns its budget instead of holding a
// connection and a database session open indefinitely.
func Timeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// SSE is long-lived by design; a timeout would sever it every d.
			if r.URL.Path == "/api/stream" {
				next.ServeHTTP(w, r)
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
