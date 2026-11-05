// flaresolverr-go is a small drop-in replacement for FlareSolverr.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/bropines/flaresolverr-go/internal/api"
	"github.com/bropines/flaresolverr-go/internal/config"
	"github.com/bropines/flaresolverr-go/internal/solver"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	if len(os.Args) > 1 && (os.Args[1] == "version" || os.Args[1] == "--version") {
		fmt.Println(version)
		return
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(log)
	log.Info("flaresolverr-go starting", "version", version, "browser", cfg.BrowserPath,
		"idleTimeout", cfg.IdleTimeout, "maxConcurrency", cfg.MaxConcurrency, "blockMedia", cfg.BlockMedia)

	mgr := solver.NewManager(cfg, log)
	srv := &http.Server{
		Addr:              net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)),
		Handler:           api.NewServer(cfg, log, mgr, version).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		log.Info("shutting down")
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()

	// read the browser version now so GET / answers without a launch
	log.Info("user agent", "userAgent", mgr.UserAgent())
	log.Info("listening", "addr", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server failed", "err", err)
		mgr.Close()
		os.Exit(1)
	}
	mgr.Close()
}

// healthcheck lets the image check itself without curl or wget.
func healthcheck() int {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8191"
	}
	c := http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get("http://127.0.0.1:" + port + "/health")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
