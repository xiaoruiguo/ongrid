package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/ongridio/ongrid/internal/pkg/tunnel"
)

// ---------------------------------------------------------------------
// loadClientConfig
// ---------------------------------------------------------------------

func TestLoadClientConfig_RequiresCloudAddr(t *testing.T) {
	t.Setenv("ONGRID_EDGE_CLOUD_ADDR", "")
	t.Setenv("ONGRID_EDGE_ACCESS_KEY", "ak")
	t.Setenv("ONGRID_EDGE_SECRET_KEY", "sk")

	_, err := loadClientConfig()
	if err == nil {
		t.Fatal("expected error for missing ONGRID_EDGE_CLOUD_ADDR")
	}
	if !containsStr(err.Error(), "ONGRID_EDGE_CLOUD_ADDR") {
		t.Errorf("err = %v, want to mention ONGRID_EDGE_CLOUD_ADDR", err)
	}
}

func TestLoadClientConfig_RequiresAccessKey(t *testing.T) {
	t.Setenv("ONGRID_EDGE_CLOUD_ADDR", "frontier:40012")
	t.Setenv("ONGRID_EDGE_ACCESS_KEY", "")
	t.Setenv("ONGRID_EDGE_SECRET_KEY", "sk")

	_, err := loadClientConfig()
	if err == nil {
		t.Fatal("expected error for missing ONGRID_EDGE_ACCESS_KEY")
	}
	if !containsStr(err.Error(), "ONGRID_EDGE_ACCESS_KEY") {
		t.Errorf("err = %v, want to mention ONGRID_EDGE_ACCESS_KEY", err)
	}
}

func TestLoadClientConfig_RequiresSecretKey(t *testing.T) {
	t.Setenv("ONGRID_EDGE_CLOUD_ADDR", "frontier:40012")
	t.Setenv("ONGRID_EDGE_ACCESS_KEY", "ak")
	t.Setenv("ONGRID_EDGE_SECRET_KEY", "")

	_, err := loadClientConfig()
	if err == nil {
		t.Fatal("expected error for missing ONGRID_EDGE_SECRET_KEY")
	}
	if !containsStr(err.Error(), "ONGRID_EDGE_SECRET_KEY") {
		t.Errorf("err = %v, want to mention ONGRID_EDGE_SECRET_KEY", err)
	}
}

func TestLoadClientConfig_DefaultHeartbeatInterval(t *testing.T) {
	t.Setenv("ONGRID_EDGE_CLOUD_ADDR", "frontier:40012")
	t.Setenv("ONGRID_EDGE_ACCESS_KEY", "ak")
	t.Setenv("ONGRID_EDGE_SECRET_KEY", "sk")
	t.Setenv("ONGRID_FRONTIER_CLIENT_HEARTBEAT_INTERVAL", "")

	cfg, err := loadClientConfig()
	if err != nil {
		t.Fatalf("loadClientConfig: %v", err)
	}
	if cfg.HeartbeatInterval != 30*time.Second {
		t.Errorf("HeartbeatInterval = %v, want 30s", cfg.HeartbeatInterval)
	}
}

func TestLoadClientConfig_CustomHeartbeatInterval(t *testing.T) {
	t.Setenv("ONGRID_EDGE_CLOUD_ADDR", "frontier:40012")
	t.Setenv("ONGRID_EDGE_ACCESS_KEY", "ak")
	t.Setenv("ONGRID_EDGE_SECRET_KEY", "sk")
	t.Setenv("ONGRID_FRONTIER_CLIENT_HEARTBEAT_INTERVAL", "5s")

	cfg, err := loadClientConfig()
	if err != nil {
		t.Fatalf("loadClientConfig: %v", err)
	}
	if cfg.HeartbeatInterval != 5*time.Second {
		t.Errorf("HeartbeatInterval = %v, want 5s", cfg.HeartbeatInterval)
	}
}

func TestLoadClientConfig_InvalidHeartbeatIntervalFallsBackToDefault(t *testing.T) {
	t.Setenv("ONGRID_EDGE_CLOUD_ADDR", "frontier:40012")
	t.Setenv("ONGRID_EDGE_ACCESS_KEY", "ak")
	t.Setenv("ONGRID_EDGE_SECRET_KEY", "sk")
	t.Setenv("ONGRID_FRONTIER_CLIENT_HEARTBEAT_INTERVAL", "not-a-duration")

	cfg, err := loadClientConfig()
	if err != nil {
		t.Fatalf("loadClientConfig: %v", err)
	}
	if cfg.HeartbeatInterval != 30*time.Second {
		t.Errorf("HeartbeatInterval = %v, want 30s fallback", cfg.HeartbeatInterval)
	}
}

// ---------------------------------------------------------------------
// localHostInfo
// ---------------------------------------------------------------------

func TestLocalHostInfo_PopulatesBasicFields(t *testing.T) {
	info := localHostInfo()
	if info.OS == "" {
		t.Error("OS is empty")
	}
	if info.Arch == "" {
		t.Error("Arch is empty")
	}
	// Hostname may be empty on minimal test sandboxes; just check it
	// doesn't panic. When set it should match os.Hostname().
	if info.Hostname != "" {
		hostname, _ := os.Hostname()
		if info.Hostname != hostname {
			t.Errorf("Hostname = %q, want %q", info.Hostname, hostname)
		}
	}
}

// ---------------------------------------------------------------------
// registerHandlers
// ---------------------------------------------------------------------

// fakeClient is a tunnel.Client test double that records handler
// registrations, OnReconnect callbacks, and Call invocations without
// touching the network. It mirrors the fake-service pattern in
// internal/manager/service/frontierbound/client_test.go.
type fakeClient struct {
	mu sync.Mutex

	handlers    map[string]tunnel.Handler
	reconnects  []func()
	calls       []fakeCall
	callResp    any
	callErr     error
	registerErr error
	closed      bool
}

type fakeCall struct {
	method string
	req    any
}

func newFakeClient() *fakeClient {
	return &fakeClient{handlers: map[string]tunnel.Handler{}}
}

func (f *fakeClient) Dial(_ context.Context) error { return nil }

func (f *fakeClient) RegisterHandler(method string, h tunnel.Handler) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[method] = h
}

func (f *fakeClient) Call(_ context.Context, method string, req, resp any) error {
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{method: method, req: req})
	f.mu.Unlock()
	if f.callErr != nil {
		return f.callErr
	}
	// callResp is nil for empty responses (HeartbeatResponse{}); for
	// non-nil callResp we marshal-then-unmarshal isn't needed because
	// the production Call path json-round-trips. For the test we
	// directly copy via json if resp is non-nil.
	if resp == nil || f.callResp == nil {
		return nil
	}
	// Use a json round-trip to populate resp like the real Call does.
	return copyViaJSON(f.callResp, resp)
}

func (f *fakeClient) AcceptStream() (tunnel.StreamConn, error) { return nil, nil }

func (f *fakeClient) OnReconnect(fn func()) {
	if fn == nil {
		return
	}
	f.mu.Lock()
	f.reconnects = append(f.reconnects, fn)
	f.mu.Unlock()
}

func (f *fakeClient) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeClient) registeredMethods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.handlers))
	for m := range f.handlers {
		out = append(out, m)
	}
	return out
}

func (f *fakeClient) lastCall() (fakeCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return fakeCall{}, false
	}
	return f.calls[len(f.calls)-1], true
}

func (f *fakeClient) reconnectCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reconnects)
}

func (f *fakeClient) fireReconnects() {
	f.mu.Lock()
	cbs := append([]func(){}, f.reconnects...)
	f.mu.Unlock()
	for _, fn := range cbs {
		fn()
	}
}

func TestRegisterHandlers_NilClientReturnsError(t *testing.T) {
	_, err := registerHandlers(nil, clientConfig{}, slog.Default())
	if err == nil {
		t.Fatal("expected error for nil client")
	}
	if !containsStr(err.Error(), "nil client") {
		t.Errorf("err = %v, want to mention nil client", err)
	}
}

func TestRegisterHandlers_NilLoggerFallsBackToDefault(t *testing.T) {
	c := newFakeClient()
	if _, err := registerHandlers(c, clientConfig{
		CloudAddr:  "frontier:40012",
		AccessKey:  "ak",
		SecretKey:  "sk",
	}, nil); err != nil {
		t.Fatalf("registerHandlers with nil logger: %v", err)
	}
}

func TestRegisterHandlers_RegistersEchoHandler(t *testing.T) {
	c := newFakeClient()
	if _, err := registerHandlers(c, clientConfig{
		CloudAddr: "frontier:40012",
		AccessKey: "ak",
		SecretKey: "sk",
	}, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("registerHandlers: %v", err)
	}
	methods := c.registeredMethods()
	found := false
	for _, m := range methods {
		if m == "echo" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("echo handler not registered; got %v", methods)
	}
}

func TestRegisterHandlers_EchoHandlerEchoesBody(t *testing.T) {
	c := newFakeClient()
	if _, err := registerHandlers(c, clientConfig{
		CloudAddr: "frontier:40012",
		AccessKey: "ak",
		SecretKey: "sk",
	}, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("registerHandlers: %v", err)
	}

	c.mu.Lock()
	echoHandler := c.handlers["echo"]
	c.mu.Unlock()
	if echoHandler == nil {
		t.Fatal("echo handler missing")
	}

	in := []byte(`{"hello":"world"}`)
	out, err := echoHandler(context.Background(), tunnel.Session{}, "echo", in)
	if err != nil {
		t.Fatalf("echo handler: %v", err)
	}
	if string(out) != string(in) {
		t.Errorf("echo out = %q, want %q", string(out), string(in))
	}
}

func TestRegisterHandlers_RegistersOneReconnectCallback(t *testing.T) {
	c := newFakeClient()
	if _, err := registerHandlers(c, clientConfig{
		CloudAddr: "frontier:40012",
		AccessKey: "ak",
		SecretKey: "sk",
	}, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("registerHandlers: %v", err)
	}
	if n := c.reconnectCount(); n != 1 {
		t.Errorf("reconnect callbacks = %d, want 1", n)
	}
}

func TestRegisterHandlers_RegisterEdgeCallsClientCall(t *testing.T) {
	c := newFakeClient()
	// Pre-populate the register_edge response.
	c.callResp = registerEdgeResponseForTest(t, 42, 1700000000)

	registerEdge, err := registerHandlers(c, clientConfig{
		CloudAddr: "frontier:40012",
		AccessKey: "ak-123",
		SecretKey: "sk-456",
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("registerHandlers: %v", err)
	}

	if err := registerEdge(); err != nil {
		t.Fatalf("registerEdge: %v", err)
	}

	last, ok := c.lastCall()
	if !ok {
		t.Fatal("no Call recorded")
	}
	if last.method != tunnel.MethodRegisterEdge {
		t.Errorf("Call method = %q, want %q", last.method, tunnel.MethodRegisterEdge)
	}
	req, ok := last.req.(tunnel.RegisterEdgeRequest)
	if !ok {
		t.Fatalf("Call req type = %T, want RegisterEdgeRequest", last.req)
	}
	if req.AccessKey != "ak-123" {
		t.Errorf("req.AccessKey = %q", req.AccessKey)
	}
	if req.SecretKey != "sk-456" {
		t.Errorf("req.SecretKey = %q", req.SecretKey)
	}
	if req.AgentVersion != version {
		t.Errorf("req.AgentVersion = %q, want %q", req.AgentVersion, version)
	}
	if req.HostInfo.OS == "" {
		t.Error("req.HostInfo.OS is empty")
	}
}

func TestRegisterHandlers_RegisterEdgePropagatesCallError(t *testing.T) {
	c := newFakeClient()
	c.callErr = errors.New("dial refused")

	registerEdge, err := registerHandlers(c, clientConfig{
		CloudAddr: "frontier:40012",
		AccessKey: "ak",
		SecretKey: "sk",
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("registerHandlers: %v", err)
	}

	err = registerEdge()
	if err == nil {
		t.Fatal("expected error from registerEdge")
	}
	if !containsStr(err.Error(), "register_edge") {
		t.Errorf("err = %v, want to mention register_edge", err)
	}
	if !containsStr(err.Error(), "dial refused") {
		t.Errorf("err = %v, want to wrap 'dial refused'", err)
	}
}

func TestRegisterHandlers_ReconnectCallbackReRegisters(t *testing.T) {
	c := newFakeClient()
	c.callResp = registerEdgeResponseForTest(t, 7, 1700000001)

	registerEdge, err := registerHandlers(c, clientConfig{
		CloudAddr: "frontier:40012",
		AccessKey: "ak",
		SecretKey: "sk",
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("registerHandlers: %v", err)
	}

	// First register.
	if err := registerEdge(); err != nil {
		t.Fatalf("registerEdge #1: %v", err)
	}

	// Fire the reconnect callback — it should re-call register_edge.
	c.fireReconnects()

	// Two calls expected: initial + reconnect.
	c.mu.Lock()
	callCount := len(c.calls)
	c.mu.Unlock()
	if callCount != 2 {
		t.Errorf("Call count after reconnect = %d, want 2", callCount)
	}
}

// ---------------------------------------------------------------------
// startHeartbeatTicker
// ---------------------------------------------------------------------

func TestStartHeartbeatTicker_NegativeIntervalDoesNothing(t *testing.T) {
	c := newFakeClient()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Negative interval should return immediately without launching a
	// goroutine; cancel the context right after to confirm no Call
	// ever lands.
	startHeartbeatTicker(ctx, c, slog.New(slog.NewTextHandler(io.Discard, nil)), -1*time.Second)

	// Give the (non-existent) goroutine a moment to misbehave.
	time.Sleep(20 * time.Millisecond)

	c.mu.Lock()
	callCount := len(c.calls)
	c.mu.Unlock()
	if callCount != 0 {
		t.Errorf("heartbeat ticker with negative interval made %d calls, want 0", callCount)
	}
}

func TestStartHeartbeatTicker_SendsHeartbeatOnTick(t *testing.T) {
	c := newFakeClient()
	c.callResp = tunnel.HeartbeatResponse{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 10ms interval so the test is fast.
	startHeartbeatTicker(ctx, c, slog.New(slog.NewTextHandler(io.Discard, nil)), 10*time.Millisecond)

	// Wait long enough for at least one tick.
	time.Sleep(50 * time.Millisecond)
	cancel()

	c.mu.Lock()
	callCount := len(c.calls)
	c.mu.Unlock()
	if callCount == 0 {
		t.Fatal("no heartbeat calls recorded")
	}
	last, ok := c.lastCall()
	if !ok {
		t.Fatal("lastCall returned false")
	}
	if last.method != tunnel.MethodHeartbeat {
		t.Errorf("Call method = %q, want %q", last.method, tunnel.MethodHeartbeat)
	}
}

// ---------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------

// registerEdgeResponseForTest builds a tunnel.RegisterEdgeResponse and
// pre-marshals it so the fakeClient's copyViaJSON path can populate a
// caller-supplied *RegisterEdgeResponse. We return the struct value so
// fakeClient.callResp can be any-typed.
func registerEdgeResponseForTest(t *testing.T, edgeID uint64, serverTime int64) tunnel.RegisterEdgeResponse {
	t.Helper()
	return tunnel.RegisterEdgeResponse{EdgeID: edgeID, ServerTime: serverTime}
}

// copyViaJSON copies src into dst via JSON round-trip, mirroring how
// tunnel.Client.Call unmarshals the response. Used by fakeClient.Call.
func copyViaJSON(src, dst any) error {
	b, err := json.Marshal(src)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
