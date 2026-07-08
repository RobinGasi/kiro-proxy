// Command kiro-proxy is an OpenAI- and Anthropic-compatible proxy in front of
// Kiro / Amazon Q Developer / AWS CodeWhisperer.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/RobinGasi/kiro-proxy/internal/config"
)

func main() {
	host := flag.String("host", "", "host to bind (overrides SERVER_HOST)")
	flag.StringVar(host, "H", "", "host to bind (shorthand)")
	port := flag.Int("port", 0, "port to bind (overrides SERVER_PORT)")
	flag.IntVar(port, "p", 0, "port to bind (shorthand)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.BoolVar(showVersion, "v", false, "print version (shorthand)")
	flag.Parse()

	if *showVersion {
		fmt.Printf("%s %s\n", config.AppTitle, config.AppVersion)
		return
	}

	cfg := config.Load()
	setupLogging(cfg.LogLevel)

	bindHost, bindPort := resolveServerConfig(cfg, *host, *port)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	d, err := buildDeps(ctx, cfg, slog.Default())
	if err != nil {
		slog.Error("startup failed", "err", err)
		os.Exit(1)
	}
	if d.accounts != nil {
		go d.accounts.StartStatePersistence(ctx)
	}

	mux := http.NewServeMux()
	d.registerRoutes(mux)

	srv := &http.Server{
		Addr:              bindHost + ":" + strconv.Itoa(bindPort),
		Handler:           mux,
		ReadHeaderTimeout: 30 * time.Second,
		// No write/idle timeout: SSE streams are long-lived. Per-request
		// deadlines are applied via context in the handlers/upstream client.
	}

	printStartupBanner(bindHost, bindPort)

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		slog.Error("server failed", "err", err)
		os.Exit(1)
	case <-ctx.Done():
		slog.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Error("graceful shutdown failed", "err", err)
		}
	}
}

func setupLogging(level string) {
	var lvl slog.Level
	switch level {
	case "TRACE", "DEBUG":
		lvl = slog.LevelDebug
	case "WARNING", "WARN":
		lvl = slog.LevelWarn
	case "ERROR", "CRITICAL":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})
	slog.SetDefault(slog.New(h))
}

// resolveServerConfig applies the CLI > ENV > default priority.
func resolveServerConfig(cfg *config.Config, cliHost string, cliPort int) (string, int) {
	host := cfg.ServerHost
	if cliHost != "" {
		host = cliHost
	}
	port := cfg.ServerPort
	if cliPort != 0 {
		port = cliPort
	}
	return host, port
}

func printStartupBanner(host string, port int) {
	display := host
	if host == "0.0.0.0" {
		display = "localhost"
	}
	slog.Info("kiro-proxy starting",
		"version", config.AppVersion,
		"addr", fmt.Sprintf("http://%s:%d", display, port),
		"health", fmt.Sprintf("http://%s:%d/health", display, port),
	)
}
