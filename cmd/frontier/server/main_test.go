package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/ongridio/ongrid/internal/manager/service/frontierbound"
	"github.com/ongridio/ongrid/internal/pkg/tunnel"
)

// ---------------------------------------------------------------------
// loadServerConfig
// ---------------------------------------------------------------------

func TestLoadServerConfig_Defaults(t *testing.T) {
	t.Setenv("ONGRID_FRONTIER_ADDR", "frontier:40011")
	t.Setenv("ONGRID_FRONTIER_SERVICE_NAME", "")
	t.Setenv("ONGRID_FRONTIER_DISABLED", "")
	t.Setenv("ONGRID_FRONTIER_SERVER_METRICS_ADDR", "")

	cfg, err := loadServerConfig()
	if err != nil {
		t.Fatalf("loadServerConfig: %v", err)
	}
	if cfg.Addr != "frontier:40011" {
		t.Errorf("Addr = %q, want frontier:40011", cfg.Addr)
	}
	if cfg.ServiceName != "ongrid-manager" {
		t.Errorf("ServiceName = %q, want ongrid-manager", cfg.ServiceName)
	}
	if cfg.Disabled {
		t.Errorf("Disabled = true, want false")
	}
	if cfg.MetricsAddr != ":9102" {
		t.Errorf("MetricsAddr = %q, want :9102", cfg.MetricsAddr)
	}
}

func TestLoadServerConfig_ExplicitEnvOverridesDefaults(t *testing.T) {
	t.Setenv("ONGRID_FRONTIER_ADDR", "broker.example.com:40011")
	t.Setenv("ONGRID_FRONTIER_SERVICE_NAME", "custom-service")
	t.Setenv("ONGRID_FRONTIER_DISABLED", "")
	t.Setenv("ONGRID_FRONTIER_SERVER_METRICS_ADDR", ":9200")

	cfg, err := loadServerConfig()
	if err != nil {
		t.Fatalf("loadServerConfig: %v", err)
	}
	if cfg.Addr != "broker.example.com:40011" {
		t.Errorf("Addr = %q", cfg.Addr)
	}
	if cfg.ServiceName != "custom-service" {
		t.Errorf("ServiceName = %q, want custom-service", cfg.ServiceName)
	}
	if cfg.MetricsAddr != ":9200" {
		t.Errorf("MetricsAddr = %q, want :9200", cfg.MetricsAddr)
	}
}

func TestLoadServerConfig_MissingAddrWhenNotDisabled(t *testing.T) {
	t.Setenv("ONGRID_FRONTIER_ADDR", "")
	t.Setenv("ONGRID_FRONTIER_DISABLED", "")

	_, err := loadServerConfig()
	if err == nil {
		t.Fatal("expected error when ONGRID_FRONTIER_ADDR is empty and not disabled")
	}
	if !errors.Is(err, nil) && !contains(err.Error(), "ONGRID_FRONTIER_ADDR") {
		t.Errorf("err = %v, want to mention ONGRID_FRONTIER_ADDR", err)
	}
}

func TestLoadServerConfig_DisabledAllowsEmptyAddr(t *testing.T) {
	t.Setenv("ONGRID_FRONTIER_ADDR", "")
	t.Setenv("ONGRID_FRONTIER_DISABLED", "true")

	cfg, err := loadServerConfig()
	if err != nil {
		t.Fatalf("loadServerConfig: %v", err)
	}
	if !cfg.Disabled {
		t.Errorf("Disabled = false, want true")
	}
	if cfg.Addr != "" {
		t.Errorf("Addr = %q, want empty", cfg.Addr)
	}
}

func TestLoadServerConfig_DisabledAcceptsOne(t *testing.T) {
	t.Setenv("ONGRID_FRONTIER_ADDR", "")
	t.Setenv("ONGRID_FRONTIER_DISABLED", "1")

	cfg, err := loadServerConfig()
	if err != nil {
		t.Fatalf("loadServerConfig: %v", err)
	}
	if !cfg.Disabled {
		t.Errorf("Disabled = false, want true for '1'")
	}
}

// ---------------------------------------------------------------------
// deriveEdgeID
// ---------------------------------------------------------------------

func TestDeriveEdgeID_IsDeterministic(t *testing.T) {
	id1 := deriveEdgeID("ak-123", "sk-456")
	id2 := deriveEdgeID("ak-123", "sk-456")
	if id1 != id2 {
		t.Fatalf("deriveEdgeID not deterministic: %d vs %d", id1, id2)
	}
}

func TestDeriveEdgeID_DifferentKeysYieldDifferentIDs(t *testing.T) {
	id1 := deriveEdgeID("ak-123", "sk-456")
	id2 := deriveEdgeID("ak-999", "sk-456")
	id3 := deriveEdgeID("ak-123", "sk-999")
	if id1 == id2 {
		t.Errorf("expected different ids for different access_key: %d == %d", id1, id2)
	}
	if id1 == id3 {
		t.Errorf("expected different ids for different secret_key: %d == %d", id1, id3)
	}
}

func TestDeriveEdgeID_EmptyAccessKeyReturnsZero(t *testing.T) {
	if id := deriveEdgeID("", "sk"); id != 0 {
		t.Errorf("deriveEdgeID('', 'sk') = %d, want 0", id)
	}
}

func TestDeriveEdgeID_NeverReturnsZeroForNonEmptyKey(t *testing.T) {
	// FNV-1a over a non-empty input can't produce 0 in practice, but
	// the function guards against it explicitly (returns 1). Exercise
	// a few keys to make sure none accidentally hit 0.
	keys := []struct{ ak, sk string }{
		{"a", ""},
		{"", "b"},
		{"access", "secret"},
		{"\x00", "\x00"},
	}
	for _, k := range keys {
		if k.ak == "" {
			continue // empty access_key short-circuits to 0 by design.
		}
		if id := deriveEdgeID(k.ak, k.sk); id == 0 {
			t.Errorf("deriveEdgeID(%q, %q) = 0, want non-zero", k.ak, k.sk)
		}
	}
}

// ---------------------------------------------------------------------
// addrString
// ---------------------------------------------------------------------

func TestAddrString_Nil(t *testing.T) {
	if got := addrString(nil); got != "" {
		t.Errorf("addrString(nil) = %q, want empty", got)
	}
}

func TestAddrString_TCPAddr(t *testing.T) {
	addr := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234}
	if got := addrString(addr); got != "127.0.0.1:1234" {
		t.Errorf("addrString = %q, want 127.0.0.1:1234", got)
	}
}

// ---------------------------------------------------------------------
// installHandlers
// ---------------------------------------------------------------------

func TestInstallHandlers_NilClientReturnsError(t *testing.T) {
	err := installHandlers(context.Background(), nil, slog.Default())
	if err == nil {
		t.Fatal("expected error for nil client")
	}
	if !contains(err.Error(), "nil client") {
		t.Errorf("err = %v, want to mention nil client", err)
	}
}

func TestInstallHandlers_DisabledClientRegistersWithoutError(t *testing.T) {
	// NewDisabled returns a Client whose Register* are no-ops; the
	// standalone binary uses this path for ONGRID_FRONTIER_DISABLED=true.
	// installHandlers must not error against it.
	c := frontierbound.NewDisabled(slog.Default())
	if err := installHandlers(context.Background(), c, slog.Default()); err != nil {
		t.Fatalf("installHandlers on disabled client: %v", err)
	}
}

func TestInstallHandlers_DisabledClientCallReturnsErrDisabled(t *testing.T) {
	// Sanity-check the disabled-client contract so the test below
	// (which relies on Call failing) isn't accidentally passing because
	// of an unrelated nil-pointer.
	c := frontierbound.NewDisabled(slog.Default())
	if err := installHandlers(context.Background(), c, slog.Default()); err != nil {
		t.Fatalf("installHandlers: %v", err)
	}
	_, err := c.Call(context.Background(), 1, tunnel.MethodRegisterEdge, []byte(`{}`))
	if err == nil {
		t.Fatal("expected ErrDisabled from Call on disabled client")
	}
	if !errors.Is(err, frontierbound.ErrDisabled) {
		t.Errorf("err = %v, want wraps ErrDisabled", err)
	}
}

func TestInstallHandlers_NilLoggerFallsBackToDefault(t *testing.T) {
	// installHandlers must not panic when log is nil — it falls back
	// to slog.Default(). The default logger writes to os.Stderr; we
	// accept the noise in test output to keep the contract explicit.
	c := frontierbound.NewDisabled(slog.Default())
	if err := installHandlers(context.Background(), c, nil); err != nil {
		t.Fatalf("installHandlers with nil logger: %v", err)
	}
}

// ---------------------------------------------------------------------
// runServer (smoke test on the disabled path)
// ---------------------------------------------------------------------

func TestRunServer_DisabledModeReturnsOnContextCancel(t *testing.T) {
	// In disabled mode the binary skips the dial; runServer should
	// start the metrics HTTP server, block on ctx, and return nil on
	// cancel. Use a random port to avoid clashing with other tests.
	t.Setenv("ONGRID_FRONTIER_SERVER_METRICS_ADDR", ":0")
	cfg := serverConfig{
		Addr:        "",
		ServiceName: "ongrid-manager",
		Disabled:    true,
		MetricsAddr: ":0", // :0 = ephemeral port; http.Server picks one.
	}

	ctx, cancel := context.WithCancel(context.Background())
	doneCh := make(chan error, 1)
	go func() { doneCh <- runServer(ctx, cfg, slog.Default()) }()

	// Give the metrics server a moment to bind, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-doneCh:
		if err != nil {
			t.Fatalf("runServer returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runServer did not return within 3s of cancel")
	}
}

// ---------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
