package tunneltest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"outbound/internal/agent"
	"outbound/internal/edge"
	"outbound/internal/tunnel"
	tunnelpb "outbound/proto"
)

type HarnessOptions struct {
	AgentID           string
	ServiceName       string
	RequestTimeout    time.Duration
	KeepaliveInterval time.Duration
	KeepaliveTimeout  time.Duration
	ReadyTimeout      time.Duration
}

type Harness struct {
	testingTB testing.TB

	agentID     string
	serviceName string
	edgeURL     string

	ctx    context.Context
	cancel context.CancelFunc

	upstreamServer *httptest.Server
	edgeHTTPServer *httptest.Server
	grpcServer     *grpc.Server
	bufListener    *bufconn.Listener
	grpcConn       *grpc.ClientConn

	transport *headerTransport

	agentMu     sync.Mutex
	agentCancel context.CancelFunc
	agentDone   chan struct{}
}

func NewHarness(tb testing.TB, upstreamHandler http.HandlerFunc, options HarnessOptions) *Harness {
	tb.Helper()

	if upstreamHandler == nil {
		upstreamHandler = func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		}
	}
	if options.AgentID == "" {
		options.AgentID = "agent-under-test"
	}
	if options.ServiceName == "" {
		options.ServiceName = "web"
	}
	if options.RequestTimeout <= 0 {
		options.RequestTimeout = time.Second
	}
	if options.KeepaliveInterval <= 0 {
		options.KeepaliveInterval = 200 * time.Millisecond
	}
	if options.KeepaliveTimeout <= 0 {
		options.KeepaliveTimeout = 200 * time.Millisecond
	}
	if options.ReadyTimeout <= 0 {
		options.ReadyTimeout = 3 * time.Second
	}

	ctx, cancel := context.WithCancel(context.Background())

	h := &Harness{
		testingTB:   tb,
		agentID:     options.AgentID,
		serviceName: options.ServiceName,
		ctx:         ctx,
		cancel:      cancel,
		agentDone:   make(chan struct{}),
	}

	h.upstreamServer = httptest.NewServer(http.HandlerFunc(upstreamHandler))
	tb.Cleanup(func() { h.Close() })

	upstreamPort := parseServerPort(tb, h.upstreamServer.URL)

	edgeServer := edge.NewServer(edge.ServerConfig{
		RequestTimeout:    options.RequestTimeout,
		KeepaliveInterval: options.KeepaliveInterval,
		KeepaliveTimeout:  options.KeepaliveTimeout,
	})

	h.bufListener = bufconn.Listen(1024 * 1024)
	h.grpcServer = grpc.NewServer()
	tunnelpb.RegisterTunnelServiceServer(h.grpcServer, edgeServer)
	go func() {
		_ = h.grpcServer.Serve(h.bufListener)
	}()

	h.edgeHTTPServer = httptest.NewServer(edgeServer)
	h.edgeURL = h.edgeHTTPServer.URL

	grpcConn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return h.bufListener.Dial()
		}),
	)
	if err != nil {
		tb.Fatalf("failed to create gRPC client: %v", err)
	}
	h.grpcConn = grpcConn

	// Use a dedicated transport with a larger connection pool so concurrent
	// tests and benchmarks do not exhaust loopback ports via TIME_WAIT.
	baseTransport := &http.Transport{
		MaxIdleConns:        256,
		MaxIdleConnsPerHost: 256,
		IdleConnTimeout:     30 * time.Second,
	}
	h.transport = &headerTransport{
		base:       baseTransport,
		agentID:    h.agentID,
		service:    h.serviceName,
		edgeURL:    h.edgeURL,
		edgeScheme: mustParseURL(tb, h.edgeURL).Scheme,
		edgeHost:   mustParseURL(tb, h.edgeURL).Host,
	}

	go h.runAgentLoop(tunnelpb.NewTunnelServiceClient(h.grpcConn), options.ServiceName, uint16(upstreamPort))

	if err := h.WaitAgentReconnected(options.ReadyTimeout); err != nil {
		tb.Fatalf("agent did not become ready: %v", err)
	}

	return h
}

func (h *Harness) Close() {
	h.cancel()

	h.agentMu.Lock()
	if h.agentCancel != nil {
		h.agentCancel()
	}
	h.agentMu.Unlock()

	select {
	case <-h.agentDone:
	case <-time.After(2 * time.Second):
	}

	if h.grpcConn != nil {
		_ = h.grpcConn.Close()
	}
	if h.grpcServer != nil {
		h.grpcServer.Stop()
	}
	if h.bufListener != nil {
		_ = h.bufListener.Close()
	}
	if h.edgeHTTPServer != nil {
		h.edgeHTTPServer.Close()
	}
	if h.upstreamServer != nil {
		h.upstreamServer.Close()
	}
}

func (h *Harness) URL(path string) string {
	if path == "" {
		path = "/"
	}
	if path[0] != '/' {
		path = "/" + path
	}
	return h.edgeURL + path
}

func (h *Harness) HTTPClient() *http.Client {
	return &http.Client{Timeout: 2 * time.Second, Transport: h.transport}
}

func (h *Harness) KillAgent() {
	h.agentMu.Lock()
	if h.agentCancel != nil {
		h.agentCancel()
	}
	h.agentMu.Unlock()
}

func (h *Harness) WaitAgentReconnected(timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	deadline := time.Now().Add(timeout)
	client := h.HTTPClient()

	// Require two consecutive successes so we don't return during the brief
	// window where the old session is torn down and the new one is just starting.
	consecutive := 0
	for {
		if time.Now().After(deadline) {
			return errors.New("timeout waiting for agent connectivity")
		}

		req, err := http.NewRequest(http.MethodGet, h.URL("/__ready"), nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusServiceUnavailable {
				consecutive++
				if consecutive >= 2 {
					return nil
				}
				continue
			}
		}
		consecutive = 0
		time.Sleep(25 * time.Millisecond)
	}
}

func (h *Harness) runAgentLoop(client tunnelpb.TunnelServiceClient, serviceName string, upstreamPort uint16) {
	defer close(h.agentDone)

	services := map[string]uint16{serviceName: upstreamPort}
	for {
		if h.ctx.Err() != nil {
			return
		}

		attemptCtx, attemptCancel := context.WithCancel(h.ctx)
		h.agentMu.Lock()
		h.agentCancel = attemptCancel
		h.agentMu.Unlock()

		stream, err := client.Connect(attemptCtx)
		if err != nil {
			attemptCancel()
			h.agentMu.Lock()
			h.agentCancel = nil
			h.agentMu.Unlock()
			select {
			case <-time.After(50 * time.Millisecond):
			case <-h.ctx.Done():
			}
			continue
		}

		agentClient := agent.NewClient(h.agentID, services)
		_ = agentClient.Run(attemptCtx, stream)
		// Cancel the attempt context so the gRPC stream closes, which causes
		// the edge to detach this session before the next attempt connects.
		attemptCancel()

		h.agentMu.Lock()
		h.agentCancel = nil
		h.agentMu.Unlock()

		select {
		case <-time.After(50 * time.Millisecond):
		case <-h.ctx.Done():
			return
		}
	}
}

func parseServerPort(tb testing.TB, rawURL string) int {
	tb.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		tb.Fatalf("failed to parse URL %q: %v", rawURL, err)
	}
	_, portString, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		tb.Fatalf("failed to split host/port from %q: %v", parsed.Host, err)
	}
	port, err := strconv.Atoi(portString)
	if err != nil {
		tb.Fatalf("failed to parse port %q: %v", portString, err)
	}
	return port
}

func mustParseURL(tb testing.TB, rawURL string) *url.URL {
	tb.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		tb.Fatalf("failed to parse URL %q: %v", rawURL, err)
	}
	return parsed
}

type headerTransport struct {
	base       http.RoundTripper
	agentID    string
	service    string
	edgeURL    string
	edgeScheme string
	edgeHost   string
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}

	clone := req.Clone(req.Context())
	clone.Header = req.Header.Clone()
	if clone.Header.Get(tunnel.HeaderAgent) == "" {
		clone.Header.Set(tunnel.HeaderAgent, t.agentID)
	}
	if clone.Header.Get(tunnel.HeaderService) == "" {
		clone.Header.Set(tunnel.HeaderService, t.service)
	}

	if clone.URL.Host == "" {
		clone.URL.Scheme = t.edgeScheme
		clone.URL.Host = t.edgeHost
	}

	if clone.URL.Scheme == "" || clone.URL.Host == "" {
		return nil, fmt.Errorf("request URL must be absolute or relative to harness edge URL %s", t.edgeURL)
	}

	return base.RoundTrip(clone)
}
