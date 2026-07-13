package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"git.gasi.dev/robin/kiro-proxy/internal/account"
	"git.gasi.dev/robin/kiro-proxy/internal/api/anthropic"
	"git.gasi.dev/robin/kiro-proxy/internal/api/openai"
	"git.gasi.dev/robin/kiro-proxy/internal/config"
	"git.gasi.dev/robin/kiro-proxy/internal/mcp"
	"git.gasi.dev/robin/kiro-proxy/internal/transport"
	"git.gasi.dev/robin/kiro-proxy/internal/upstream"
)

// deps holds the shared dependencies wired into the HTTP handlers.
type deps struct {
	cfg      *config.Config
	accounts *account.Manager
	upstream *upstream.Client
	mcp      *mcp.Client
	logger   *slog.Logger
}

// buildDeps constructs the shared dependency graph: a single shared HTTP client
// (honoring VPN_PROXY_URL), the upstream Kiro client, the multi-account manager
// (with its model-list fetcher pointed at upstream), and the MCP client.
//
// A failure to initialize accounts is logged but not fatal: the server still
// serves /health, and request routes return 503 until credentials are valid.
func buildDeps(ctx context.Context, cfg *config.Config, logger *slog.Logger) (*deps, error) {
	hc, err := transport.New(cfg.VPNProxyURL)
	if err != nil {
		return nil, err
	}
	up := upstream.New(hc, cfg, logger)

	accounts, err := account.New(ctx, cfg, hc, logger)
	if err != nil {
		logger.Error("account manager init failed; /v1 routes will return 503 until fixed", "err", err)
	}
	if accounts != nil {
		accounts.SetModelFetcher(func(ctx context.Context, a *account.Account) ([]string, error) {
			return up.ListAvailableModels(ctx, a.Auth())
		})
	}

	return &deps{
		cfg:      cfg,
		accounts: accounts,
		upstream: up,
		mcp:      mcp.NewClient(hc, logger),
		logger:   logger,
	}, nil
}

// registerRoutes wires the full HTTP surface onto mux.
func (d *deps) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /", healthRoot)
	mux.HandleFunc("GET /health", healthDetail)

	if d.accounts == nil {
		// No usable accounts: only health is served.
		return
	}

	anthropic.NewHandler(anthropic.Deps{
		Cfg:      d.cfg,
		Accounts: d.accounts,
		Upstream: d.upstream,
		MCP:      d.mcp,
		Logger:   d.logger,
	}).RegisterRoutes(mux)

	openai.NewHandler(openai.Deps{
		Cfg:      d.cfg,
		Accounts: d.accounts,
		Upstream: d.upstream,
		MCP:      d.mcp,
		Logger:   d.logger,
	}).RegisterRoutes(mux)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func healthRoot(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"service": config.AppTitle,
		"version": config.AppVersion,
	})
}

func healthDetail(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "healthy",
		"service": config.AppTitle,
		"version": config.AppVersion,
	})
}
