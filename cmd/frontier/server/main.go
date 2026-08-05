// Command frontier-server is a standalone service-bound client of the
// upstream frontier broker (github.com/singchia/frontier). It mirrors the
// cloud-side wiring in cmd/ongrid (see ongrid_frontier.md §servicebound)
// but ships only the frontierbound layer — no HTTP API, no DB, no AIOps —
// so it can be used as a development harness and a smoke-test binary for
// the tunnel topology:
//
//	edge (frontier-client) ──▶ frontier:40012 (edgebound)
//	                                  │
//	                                  ▼
//	frontier-server ──▶ frontier:40011 (servicebound)
//
// The binary reads its configuration from env vars (the same names as
// cmd/ongrid, see loadServerConfig), dials the broker via
// frontierbound.New, installs a minimal handler set (GetEdgeID +
// register_edge echo + lifecycle loggers) via installHandlers, and
// blocks on SIGINT/SIGTERM. It exposes /healthz + /metrics on the
// debug listener (ONGRID_FRONTIER_SERVER_METRICS_ADDR, default :9102)
// following the AGENTS.md "所有对外服务必须暴露 /healthz、/readyz、/metrics"
// red line.
//
// This binary intentionally does NOT implement the full manager Wiring
// (EdgeAuthn / EdgeUC / MetricIngester / ...). It is a thin shell for
// testing the frontierbound service registration path in isolation; the
// production cloud path remains cmd/ongrid.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ongridio/ongrid/internal/manager/service/frontierbound"
	"github.com/ongridio/ongrid/internal/pkg/tunnel"
)

// version is overwritten at build time via -ldflags "-X main.version=$(VERSION)".
var version = "dev"

// serverConfig is the env-driven configuration for the frontier-server
// binary. It is a strict subset of cmd/ongrid's FrontierClientConfig —
// only the fields the frontierbound layer needs.
type serverConfig struct {
	// Addr is the frontier service-bound listen, e.g. "frontier:40011"
	// or "127.0.0.1:40011". Required unless Disabled.
	Addr string
	// ServiceName identifies this service to the frontier broker;
	// reported via fbsvc.OptionServiceName so the broker can route by
	// service. Defaults to "ongrid-manager" to match cmd/ongrid.
	ServiceName string
	// Disabled skips the dial entirely (matches cmd/ongrid's
	// ONGRID_FRONTIER_DISABLED degraded-broker mode).
	Disabled bool
	// MetricsAddr is the local debug listener for /healthz + /metrics.
	// Defaults to ":9102" (kept off :9100 which cmd/ongrid owns).
	MetricsAddr string
}

// loadServerConfig reads serverConfig from environment variables. It
// returns an error only when a required field is missing AND the binary
// is not in disabled mode; missing optional fields fall back to the
// documented defaults.
func loadServerConfig() (serverConfig, error) {
	cfg := serverConfig{
		Addr:        os.Getenv("ONGRID_FRONTIER_ADDR"),
		ServiceName: getEnvDefault("ONGRID_FRONTIER_SERVICE_NAME", "ongrid-manager"),
		Disabled:    os.Getenv("ONGRID_FRONTIER_DISABLED") == "true" || os.Getenv("ONGRID_FRONTIER_DISABLED") == "1",
		MetricsAddr: getEnvDefault("ONGRID_FRONTIER_SERVER_METRICS_ADDR", ":9102"),
	}
	if !cfg.Disabled && cfg.Addr == "" {
		return cfg, errors.New("frontier-server: ONGRID_FRONTIER_ADDR is required (or set ONGRID_FRONTIER_DISABLED=true)")
	}
	return cfg, nil
}

func getEnvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// installHandlers wires the minimal handler set the frontier-server
// binary needs to participate in the tunnel topology:
//
//   - GetEdgeID: parse Meta {access_key, secret_key} and return a stable
//     canonical edge_id derived from the access_key hash. Production
//     cmd/ongrid delegates this to AccessKeyAuthenticator; the standalone
//     binary uses a deterministic mapping so a given key always resolves
//     to the same edge_id across reconnects (required by the frontier
//     binding rebuild path documented in ongrid_frontier.md §red-lines).
//   - EdgeOnline / EdgeOffline: structured log lines; no state mutation
//     (this binary owns no DB).
//   - register_edge: echo the edge_id back in RegisterEdgeResponse so
//     a paired frontier-client can verify end-to-end RPC delivery.
//   - heartbeat: ack with an empty HeartbeatResponse.
//
// The function is idempotent: calling it twice on the same Client
// re-registers the handlers (frontierbound.Client.Register replaces the
// prior handler).
func installHandlers(ctx context.Context, c *frontierbound.Client, log *slog.Logger) error {
	if c == nil {
		return errors.New("frontier-server: nil client")
	}
	if log == nil {
		log = slog.Default()
	}

	if err := c.RegisterGetEdgeID(ctx, func(meta []byte) (uint64, error) {
		var m tunnel.Meta
		if err := json.Unmarshal(meta, &m); err != nil {
			return 0, fmt.Errorf("frontier-server: decode meta: %w", err)
		}
		if m.AccessKey == "" || m.SecretKey == "" {
			return 0, errors.New("frontier-server: missing credentials in meta")
		}
		// Deterministic edge_id from access_key so reconnects re-bind
		// the same canonical id (mirrors AccessKeyAuthenticator's
		// DB-PK contract but without the DB).
		return deriveEdgeID(m.AccessKey, m.SecretKey), nil
	}); err != nil {
		return fmt.Errorf("frontier-server: RegisterGetEdgeID: %w", err)
	}

	if err := c.RegisterEdgeOnline(ctx, func(edgeID uint64, meta []byte, addr net.Addr) error {
		log.Info("edge online",
			slog.Uint64("edge_id", edgeID),
			slog.String("addr", addrString(addr)),
		)
		return nil
	}); err != nil {
		return fmt.Errorf("frontier-server: RegisterEdgeOnline: %w", err)
	}

	if err := c.RegisterEdgeOffline(ctx, func(edgeID uint64, meta []byte, addr net.Addr) error {
		log.Info("edge offline",
			slog.Uint64("edge_id", edgeID),
			slog.String("addr", addrString(addr)),
		)
		return nil
	}); err != nil {
		return fmt.Errorf("frontier-server: RegisterEdgeOffline: %w", err)
	}

	if err := c.Register(ctx, tunnel.MethodRegisterEdge, func(ctx context.Context, edgeID uint64, body []byte) ([]byte, error) {
		var req tunnel.RegisterEdgeRequest
		if err := json.Unmarshal(body, &req); err != nil {
			// Echo the edge_id back even on malformed body so the
			// client can complete its handshake.
			log.Warn("register_edge: malformed body", slog.Any("err", err))
		}
		log.Info("register_edge",
			slog.Uint64("edge_id", edgeID),
			slog.String("hostname", req.HostInfo.Hostname),
			slog.String("agent_version", req.AgentVersion),
			slog.Int("body_bytes", len(body)),
		)
		resp := tunnel.RegisterEdgeResponse{
			EdgeID:     edgeID,
			ServerTime: time.Now().UTC().Unix(),
		}
		out, err := json.Marshal(resp)
		if err != nil {
			return nil, fmt.Errorf("frontier-server: marshal register_edge resp: %w", err)
		}
		return out, nil
	}); err != nil {
		return fmt.Errorf("frontier-server: Register %q: %w", tunnel.MethodRegisterEdge, err)
	}

	if err := c.Register(ctx, tunnel.MethodHeartbeat, func(ctx context.Context, edgeID uint64, body []byte) ([]byte, error) {
		// Heartbeat ack is empty; logging at debug to avoid spam.
		log.Debug("heartbeat", slog.Uint64("edge_id", edgeID))
		out, err := json.Marshal(tunnel.HeartbeatResponse{})
		if err != nil {
			return nil, fmt.Errorf("frontier-server: marshal heartbeat resp: %w", err)
		}
		return out, nil
	}); err != nil {
		return fmt.Errorf("frontier-server: Register %q: %w", tunnel.MethodHeartbeat, err)
	}

	return nil
}

// addrString safely formats a net.Addr, handling the nil case that
// frontier may pass to the offline callback during shutdown.
func addrString(addr net.Addr) string {
	if addr == nil {
		return ""
	}
	return addr.String()
}

// deriveEdgeID returns a deterministic uint64 derived from the
// access_key + secret_key pair. The exact algorithm doesn't matter for
// the standalone binary; what matters is that the same pair always
// resolves to the same id so the frontier binding rebuild path can
// re-associate a reconnecting edge with its canonical id (see
// ongrid_frontier.md §red-lines rule 3: "canonicalizeEdgeID does NOT
// fall back to transport ID").
//
// We avoid crypto/sha256 to keep the dependency surface minimal; FNV-1a
// over the concatenated key gives a stable 64-bit value.
func deriveEdgeID(accessKey, secretKey string) uint64 {
	if accessKey == "" {
		return 0
	}
	// Simple FNV-1a 64-bit over access_key||":"||secret_key.
	const (
		offset uint64 = 1469598103934665603
		prime  uint64 = 1099511628211
	)
	h := offset
	for i := 0; i < len(accessKey); i++ {
		h ^= uint64(accessKey[i])
		h *= prime
	}
	h ^= ':'
	h *= prime
	for i := 0; i < len(secretKey); i++ {
		h ^= uint64(secretKey[i])
		h *= prime
	}
	if h == 0 {
		return 1 // 0 is the "unknown edge" sentinel in frontierbound.
	}
	return h
}

// runServer is the testable entry point: it takes an explicit config and
// logger, builds the frontierbound.Client, installs handlers, and blocks
// until ctx is cancelled. Splitting run out of main lets tests drive
// the binary with an injected context without exec'ing a subprocess.
func runServer(ctx context.Context, cfg serverConfig, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}

	var client *frontierbound.Client
	if cfg.Disabled {
		log.Warn("frontier-server: disabled (ONGRID_FRONTIER_DISABLED=true) — all calls will return ErrDisabled")
		client = frontierbound.NewDisabled(log.With(slog.String("comp", "frontierbound")))
	} else {
		c, err := frontierbound.New(frontierbound.Config{
			Addr:        cfg.Addr,
			ServiceName: cfg.ServiceName,
		}, log.With(slog.String("comp", "frontierbound")))
		if err != nil {
			return fmt.Errorf("frontier-server: frontierbound.New: %w", err)
		}
		client = c
	}
	defer func() {
		if err := client.Close(); err != nil {
			log.Warn("frontier-server: client close", slog.Any("err", err))
		}
	}()

	if err := installHandlers(ctx, client, log); err != nil {
		return fmt.Errorf("frontier-server: install handlers: %w", err)
	}

	log.Info("frontier-server ready",
		slog.String("addr", cfg.Addr),
		slog.String("service_name", cfg.ServiceName),
		slog.Bool("disabled", cfg.Disabled),
		slog.String("version", version),
	)

	// Debug HTTP surface: /healthz + /metrics. /readyz flips to 200 only
	// after installHandlers completes (matches AGENTS.md red line).
	ready := &atomic.Bool{}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if ready.Load() {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ready\n"))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("not ready\n"))
	})
	mux.Handle("/metrics", promhttp.Handler())

	srv := &http.Server{
		Addr:              cfg.MetricsAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	ready.Store(true)

	errCh := make(chan error, 2)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("frontier-server: metrics server: %w", err)
		}
	}()

	select {
	case <-ctx.Done():
		log.Info("frontier-server shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Warn("frontier-server: metrics shutdown", slog.Any("err", err))
		}
		return nil
	case err := <-errCh:
		return err
	}
}

func main() {
	for _, a := range os.Args[1:] {
		switch a {
		case "--version", "-v":
			fmt.Fprintf(os.Stdout, "frontier-server %s\n", version)
			return
		case "--help", "-h":
			fmt.Fprintf(os.Stdout, "frontier-server %s\n", version)
			fmt.Fprintln(os.Stdout, "")
			fmt.Fprintln(os.Stdout, "Standalone service-bound client of the frontier broker.")
			fmt.Fprintln(os.Stdout, "")
			fmt.Fprintln(os.Stdout, "Configuration (env):")
			fmt.Fprintln(os.Stdout, "  ONGRID_FRONTIER_ADDR                  frontier service-bound addr (required, e.g. frontier:40011)")
			fmt.Fprintln(os.Stdout, "  ONGRID_FRONTIER_SERVICE_NAME          service name (default ongrid-manager)")
			fmt.Fprintln(os.Stdout, "  ONGRID_FRONTIER_DISABLED              true|1 to skip dial (degraded mode)")
			fmt.Fprintln(os.Stdout, "  ONGRID_FRONTIER_SERVER_METRICS_ADDR   debug listener (default :9102)")
			return
		}
	}

	fmt.Fprintf(os.Stderr, "frontier-server %s starting\n", version)

	cfg, err := loadServerConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	rootCtx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := runServer(rootCtx, cfg, log); err != nil {
		log.Error("frontier-server exited with error", slog.Any("err", err))
		os.Exit(1)
	}
}
