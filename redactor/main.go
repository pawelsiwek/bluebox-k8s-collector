// Command k8s-mcp-redactor sits in front of kubernetes-mcp-server inside the
// customer cluster: it lets only read-only tool calls through and redacts env
// values, command/args, probe headers and credential fields from every result
// (contract C1/C2, docs/design/2026-10-26-k8s-onboarding-contracts.md).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// config is the redactor's whole command line: contract C2 runs it as a sidecar
// with --listen 127.0.0.1:8080 --upstream <kubernetes-mcp-server URL>.
type config struct {
	listen   string
	upstream string
}

func parseFlags(args []string) (config, error) {
	fs := flag.NewFlagSet("k8s-mcp-redactor", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // the caller logs the returned error
	var c config
	fs.StringVar(&c.listen, "listen", "127.0.0.1:8080", "address to serve /mcp on")
	fs.StringVar(&c.upstream, "upstream", "", "kubernetes-mcp-server MCP endpoint URL (required)")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if fs.NArg() > 0 {
		return config{}, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	u, err := url.Parse(c.upstream)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return config{}, errors.New("--upstream must be an http(s) URL")
	}
	if _, _, err := net.SplitHostPort(c.listen); err != nil {
		return config{}, fmt.Errorf("--listen: %w", err)
	}
	return c, nil
}

func newMux(p *proxy) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/mcp", p)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := parseFlags(os.Args[1:])
	if err != nil {
		log.Error("invalid command line", "error", err)
		os.Exit(2)
	}
	addr, upstream := cfg.listen, cfg.upstream

	srv := &http.Server{
		Addr:              addr,
		Handler:           newMux(newProxy(upstream, log)),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      upstreamTimeout + 10*time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Error("shutdown", "error", err)
		}
	}()

	log.Info("k8s-mcp-redactor listening", "addr", addr, "upstream", upstream)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server failed", "error", err)
		os.Exit(1)
	}
}
