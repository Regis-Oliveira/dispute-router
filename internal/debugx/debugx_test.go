package debugx

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Off by default: an empty address is "not enabled", not an error, so the
// call can sit in an errgroup without changing how the group ends.
func TestAnEmptyAddressIsOff(t *testing.T) {
	if err := Serve(context.Background(), "", slog.Default()); err != nil {
		t.Fatalf("Serve(\"\") = %v, want nil", err)
	}
}

// On, it answers the pprof index on loopback and stops when the context does.
func TestItServesPprofAndStopsWithTheContext(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick a port: %v", err)
	}
	addr := listener.Addr().String()
	listener.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, addr, slog.Default()) }()

	var res *http.Response
	for range 50 {
		res, err = http.Get("http://" + addr + "/debug/pprof/")
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("diagnostics never answered: %v", err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), "goroutine") {
		t.Fatalf("pprof index: status %d, body %q", res.StatusCode, string(body[:min(len(body), 80)]))
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v after cancel, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not stop when the context was cancelled")
	}
}
