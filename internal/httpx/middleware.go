// Package httpx holds the transport concerns that are not specific to any one
// endpoint.
package httpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"
)

type ctxKey int

const requestIDKey ctxKey = iota

// RequestID pulls the current request's id out of a context, for log lines
// written deeper in the stack.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

func newRequestID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(buf[:])
}

// statusRecorder remembers what the handler actually wrote, since
// http.ResponseWriter will not tell you afterwards.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// Unwrap lets http.NewResponseController reach the real ResponseWriter.
//
// Wrapping a ResponseWriter hides every optional interface it implements -
// Flusher, Hijacker, ReadFrom - because the wrapper only satisfies
// ResponseWriter itself. That is why the SSE endpoint saw "streaming
// unsupported" the moment this middleware was in front of it: the type
// assertion was against the recorder, not the writer underneath. Implementing
// Unwrap is the supported way through, and it keeps working for interfaces
// nobody here has thought about yet.
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// Middleware tags each request with an id, logs the outcome, and stops a panic
// in one handler from taking the process down.
func Middleware(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get("X-Request-Id")
			if id == "" {
				id = newRequestID()
			}
			w.Header().Set("X-Request-Id", id)

			ctx := context.WithValue(r.Context(), requestIDKey, id)
			recorder := &statusRecorder{ResponseWriter: w}
			started := time.Now()

			defer func() {
				if recovered := recover(); recovered != nil {
					logger.ErrorContext(ctx, "handler panicked",
						"panic", recovered, "request_id", id, "path", r.URL.Path)
					if recorder.status == 0 {
						http.Error(recorder, `{"error":"internal error"}`, http.StatusInternalServerError)
					}
				}
				// The query is logged, not only the path. Every filter this API
				// takes lives in the query string, so a log without it cannot
				// answer the one question worth asking of a read endpoint:
				// what was actually asked for. It is safe here because these
				// filters carry states and merchant ids - if a parameter ever
				// carries something personal, this line is where to redact it.
				logger.InfoContext(ctx, "request",
					"request_id", id,
					"method", r.Method,
					"path", r.URL.Path,
					"query", r.URL.RawQuery,
					"status", recorder.status,
					"duration_ms", time.Since(started).Milliseconds())
			}()

			next.ServeHTTP(recorder, r.WithContext(ctx))
		})
	}
}
