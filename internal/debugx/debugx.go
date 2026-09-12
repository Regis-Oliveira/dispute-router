// Package debugx serves the Go runtime's own diagnostics for one process.
//
// net/http/pprof exposes CPU and heap profiles, goroutine dumps and the
// execution tracer over HTTP. It is served on its own listener rather than
// mounted on a service's public mux, and bound to loopback, so nothing about
// the process's internals is reachable from anyone but a shell on the same
// machine. Empty address means off, which is the default: a diagnostics
// endpoint is a thing you turn on when you have a question.
//
// The one that answers "is this actually parallel" is the execution trace:
//
//	curl -o trace.out 'http://127.0.0.1:6061/debug/pprof/trace?seconds=10'
//	go tool trace trace.out
//
// which shows, per logical processor, which goroutine ran when, where it
// blocked on the network, and every GC cycle.
package debugx

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"time"
)

// Serve runs the diagnostics server until ctx is cancelled. A nil error on an
// empty addr means "not enabled", so it sits in an errgroup beside the real
// servers without changing how the group ends.
func Serve(ctx context.Context, addr string, logger *slog.Logger) error {
	if addr == "" {
		return nil
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		// No WriteTimeout: a trace or a CPU profile is a long response by
		// design, ?seconds=30 is a normal request.
	}

	done := make(chan error, 1)
	go func() {
		logger.Info("diagnostics listening", "addr", addr, "hint", "curl 'http://"+addr+"/debug/pprof/trace?seconds=10'")
		err := server.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		done <- err
	}()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		return nil
	}
}
