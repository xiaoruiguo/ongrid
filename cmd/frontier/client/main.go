// Command frontier-client is a standalone edge-bound client of the
// upstream frontier broker (github.com/singchia/frontier). It mirrors
// the edge-side wiring in cmd/ongrid-edge (see ongrid_frontier.md
// §edgebound) but ships only the tunnel layer — no collectors, no
// plugins, no skill registry — so it can be used as a development
// harness and a smoke-test binary for the tunnel topology:
//
//	frontier-client ──▶ frontier:40012 (edgebound)
//	                          │
//	                          ▼
//	frontier-server ──▶ frontier:40011 (servicebound)
//
// The binary reads its configuration from env vars (the same names as
// cmd/ongrid-edge, see loadClientConfig), constructs a tunnel.Client,
// dials the broker, registers a minimal handler set (echo + heartbeat
// tick) via registerHandlers, and blocks on SIGINT/SIGTERM.
//
// This binary intentionally does NOT implement the full edge agent
// (collectors / plugins / webshell / skills). It is a thin shell for
// testing the tunnel client dial + reconnect + reverse-RPC path in
// isolation; the production edge path remains cmd/ongrid-edge.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/ongridio/ongrid/internal/pkg/tunnel"
)

// version is overwritten at build time via -ldflags "-X main.version=$(VERSION)".
var version = "dev"

// clientConfig is the env-driven configuration for the frontier-client
// binary. It is a strict subset of cmd/ongrid-edge's edge config — only
// the fields the tunnel.Client needs.
type clientConfig struct {
	// CloudAddr is the frontier edge-bound listen, e.g. "frontier:40012"
	// or "127.0.0.1:40012". Required.
	CloudAddr string
	// AccessKey / SecretKey are presented in the geminio Meta blob on
	// every (re)connect. The frontier broker forwards the Meta to the
	// service-side GetEdgeID callback, which maps it to a canonical
	// edge_id.
	AccessKey string
	SecretKey string
	// HeartbeatInterval is how often the client sends a heartbeat RPC
	// after a successful register_edge. Defaults to 30s (matches the
	// production edge agent default).
	HeartbeatInterval time.Duration
}

// loadClientConfig reads clientConfig from environment variables. It
// returns an error when a required field is missing.
func loadClientConfig() (clientConfig, error) {
	cfg := clientConfig{
		CloudAddr: os.Getenv("ONGRID_EDGE_CLOUD_ADDR"),
		AccessKey: os.Getenv("ONGRID_EDGE_ACCESS_KEY"),
		SecretKey: os.Getenv("ONGRID_EDGE_SECRET_KEY"),
	}
	if cfg.CloudAddr == "" {
		return cfg, errors.New("frontier-client: ONGRID_EDGE_CLOUD_ADDR is required")
	}
	if cfg.AccessKey == "" {
		return cfg, errors.New("frontier-client: ONGRID_EDGE_ACCESS_KEY is required")
	}
	if cfg.SecretKey == "" {
		return cfg, errors.New("frontier-client: ONGRID_EDGE_SECRET_KEY is required")
	}
	cfg.HeartbeatInterval = getEnvDuration("ONGRID_FRONTIER_CLIENT_HEARTBEAT_INTERVAL", 30*time.Second)
	return cfg, nil
}

func getEnvDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

// registerHandlers installs the minimal reverse-RPC handler set the
// frontier-client binary needs to participate in the tunnel topology:
//
//   - "echo": returns the request body unchanged. Used by the paired
//     frontier-server (or any service-bound client) to verify the
//     cloud→edge RPC path is live.
//
// The function also wires an OnReconnect callback that re-issues
// register_edge on every reconnect — this is the red-line documented
// in ongrid_frontier.md §red-lines rule 7: "RegisterHandler is called
// BEFORE Dial (avoids missing first RPCs)" and rule 14: the edge must
// re-register on every new transport so the cloud can rebuild its
// transportID→edge_id map.
//
// registerHandlers returns the register_edge request function so the
// caller can invoke it once after Dial and again from OnReconnect.
func registerHandlers(c tunnel.Client, cfg clientConfig, log *slog.Logger) (registerEdge func() error, err error) {
	if c == nil {
		return nil, errors.New("frontier-client: nil client")
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	// echo handler — the simplest possible cloud→edge round-trip probe.
	c.RegisterHandler("echo", func(ctx context.Context, _ tunnel.Session, method string, body []byte) ([]byte, error) {
		log.Debug("echo", slog.Int("body_bytes", len(body)))
		// Echo the body back verbatim.
		return append([]byte(nil), body...), nil
	})

	// registerEdge sends register_edge with the local HostInfo. It is
	// called once after Dial succeeds and again on every OnReconnect so
	// the cloud can re-bind canonical edge_id to the new transport id.
	hostInfo := localHostInfo()
	registerEdge = func() error {
		req := tunnel.RegisterEdgeRequest{
			AccessKey:    cfg.AccessKey,
			SecretKey:    cfg.SecretKey,
			HostInfo:     hostInfo,
			AgentVersion: version,
		}
		var resp tunnel.RegisterEdgeResponse
		if err := c.Call(context.Background(), tunnel.MethodRegisterEdge, req, &resp); err != nil {
			return fmt.Errorf("frontier-client: register_edge: %w", err)
		}
		log.Info("register_edge ok",
			slog.Uint64("edge_id", resp.EdgeID),
			slog.Int64("server_time", resp.ServerTime),
		)
		return nil
	}

	// OnReconnect: re-issue register_edge. The tunnel client already
	// re-primes reverse-RPC handlers (geminio RetryEnd memorizes them),
	// so only the application-level register_edge needs to be re-sent.
	c.OnReconnect(func() {
		log.Info("frontier-client reconnected; re-registering edge")
		if err := registerEdge(); err != nil {
			log.Warn("frontier-client: re-register after reconnect failed", slog.Any("err", err))
		}
	})

	return registerEdge, nil
}

// localHostInfo builds a tunnel.HostInfo describing the host the client
// is running on. The standalone binary only populates the cheap fields
// (hostname / os / arch); the production edge agent additionally
// collects CPU count, memory, kernel version, and fingerprints via
// gopsutil — out of scope for this smoke-test binary. The agent version
// travels on RegisterEdgeRequest.AgentVersion, not HostInfo.
func localHostInfo() tunnel.HostInfo {
	hostname, _ := os.Hostname()
	return tunnel.HostInfo{
		Hostname: hostname,
		OS:       runtime.GOOS,
		Arch:     runtime.GOARCH,
	}
}

// startHeartbeatTicker launches a goroutine that sends heartbeat RPCs
// at cfg.HeartbeatInterval until ctx is cancelled. The first tick is
// delayed by the full interval so an initial heartbeat doesn't race
// with the post-Dial register_edge call.
func startHeartbeatTicker(ctx context.Context, c tunnel.Client, log *slog.Logger, interval time.Duration) {
	if interval <= 0 {
		return
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				req := tunnel.HeartbeatRequest{
					Ts: time.Now().UTC().Unix(),
				}
				var resp tunnel.HeartbeatResponse
				if err := c.Call(ctx, tunnel.MethodHeartbeat, req, &resp); err != nil {
					log.Warn("frontier-client: heartbeat", slog.Any("err", err))
					continue
				}
				log.Debug("frontier-client: heartbeat ok")
			}
		}
	}()
}

// runClient is the testable entry point: it takes an explicit config and
// logger, builds the tunnel.Client, dials, registers handlers, and
// blocks until ctx is cancelled. Splitting run out of main lets tests
// drive the binary with an injected context without exec'ing a
// subprocess. It does NOT perform the network dial in unit tests —
// tests cover loadClientConfig and registerHandlers separately.
func runClient(ctx context.Context, cfg clientConfig, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}

	client := tunnel.NewClient(tunnel.ClientConfig{
		CloudAddr:  cfg.CloudAddr,
		AccessKey:  cfg.AccessKey,
		SecretKey:  cfg.SecretKey,
		Log:        log.With(slog.String("comp", "tunnel")),
	})

	registerEdge, err := registerHandlers(client, cfg, log)
	if err != nil {
		return fmt.Errorf("frontier-client: register handlers: %w", err)
	}

	log.Info("frontier-client dialing", slog.String("cloud_addr", cfg.CloudAddr))
	if err := client.Dial(ctx); err != nil {
		return fmt.Errorf("frontier-client: dial: %w", err)
	}

	if err := registerEdge(); err != nil {
		return fmt.Errorf("frontier-client: initial register_edge: %w", err)
	}

	startHeartbeatTicker(ctx, client, log, cfg.HeartbeatInterval)

	log.Info("frontier-client ready",
		slog.String("cloud_addr", cfg.CloudAddr),
		slog.Duration("heartbeat_interval", cfg.HeartbeatInterval),
		slog.String("version", version),
	)

	<-ctx.Done()
	log.Info("frontier-client shutting down")
	if err := client.Close(); err != nil {
		log.Warn("frontier-client: close", slog.Any("err", err))
	}
	return nil
}

func main() {
	for _, a := range os.Args[1:] {
		switch a {
		case "--version", "-v":
			fmt.Fprintf(os.Stdout, "frontier-client %s\n", version)
			return
		case "--help", "-h":
			fmt.Fprintf(os.Stdout, "frontier-client %s\n", version)
			fmt.Fprintln(os.Stdout, "")
			fmt.Fprintln(os.Stdout, "Standalone edge-bound client of the frontier broker.")
			fmt.Fprintln(os.Stdout, "")
			fmt.Fprintln(os.Stdout, "Configuration (env):")
			fmt.Fprintln(os.Stdout, "  ONGRID_EDGE_CLOUD_ADDR                       frontier edge-bound addr (required, e.g. frontier:40012)")
			fmt.Fprintln(os.Stdout, "  ONGRID_EDGE_ACCESS_KEY                       edge access key (required)")
			fmt.Fprintln(os.Stdout, "  ONGRID_EDGE_SECRET_KEY                       edge secret key (required)")
			fmt.Fprintln(os.Stdout, "  ONGRID_FRONTIER_CLIENT_HEARTBEAT_INTERVAL   heartbeat interval (default 30s)")
			return
		}
	}

	fmt.Fprintf(os.Stderr, "frontier-client %s starting\n", version)

	cfg, err := loadClientConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	rootCtx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := runClient(rootCtx, cfg, log); err != nil {
		log.Error("frontier-client exited with error", slog.Any("err", err))
		os.Exit(1)
	}
}
