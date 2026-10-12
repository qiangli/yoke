package stack

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	collog "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collmetric "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	colltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// otlpGRPCBridge is a loopback-only OTLP/gRPC receiver that forwards to the
// same Victoria stores as the HTTP proxy. It exists for Kopia v0.23.1, whose
// --otlp-trace uses otlptracegrpc (Go gRPC exporter, see
// cli/observability_flags.go: otlptracegrpc.NewUnstarted) and therefore does
// not speak OTLP/HTTP. The bridge listens on 127.0.0.1:31416 (port next to
// 31415) and POSTs protobuf to the loopback HTTP backends; it never binds
// non-loopback and never forwards to a vendor endpoint.
type otlpGRPCBridge struct {
	addr    string
	backend map[string]string // "/v1/traces" -> "http://127.0.0.1:<port>/insert/..."

	mu       sync.Mutex
	listener net.Listener
	server   *grpc.Server
	client   *http.Client
}

var _ Component = (*otlpGRPCBridge)(nil)

// NewOTLPGRPCBridge creates the gRPC bridge. addr must be loopback (127.0.0.1:port);
// otlpRoutes provides the HTTP backend URLs for each signal.
func NewOTLPGRPCBridge(addr string, otlpRoutes map[string]string) *otlpGRPCBridge {
	backend := make(map[string]string, len(otlpRoutes))
	for k, v := range otlpRoutes {
		backend[k] = v
	}
	return &otlpGRPCBridge{
		addr:    addr,
		backend: backend,
		client:  &http.Client{Timeout: 10 * time.Second},
	}
}

func (b *otlpGRPCBridge) Name() string { return "otlp-grpc-bridge" }

func (b *otlpGRPCBridge) Start(_ context.Context) error {
	if !strings.HasPrefix(b.addr, "127.0.0.1:") && !strings.HasPrefix(b.addr, "localhost:") {
		return fmt.Errorf("otlp-grpc-bridge: refusing non-loopback bind %q (must be 127.0.0.1)", b.addr)
	}
	if b.backend == nil {
		b.backend = map[string]string{}
	}
	ln, err := net.Listen("tcp", b.addr)
	if err != nil {
		return fmt.Errorf("otlp-grpc-bridge listen %s: %w", b.addr, err)
	}
	s := grpc.NewServer()
	colltrace.RegisterTraceServiceServer(s, &grpcTraceServer{bridge: b})
	collog.RegisterLogsServiceServer(s, &grpcLogsServer{bridge: b})
	collmetric.RegisterMetricsServiceServer(s, &grpcMetricsServer{bridge: b})

	b.mu.Lock()
	b.listener = ln
	b.server = s
	b.mu.Unlock()

	slog.Info("otlp-grpc-bridge: starting", "addr", b.addr)
	go func() {
		if err := s.Serve(ln); err != nil {
			slog.Error("otlp-grpc-bridge: serve failed", "error", err)
		}
	}()
	return nil
}

func (b *otlpGRPCBridge) Stop(_ context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.server != nil {
		b.server.GracefulStop()
		b.server = nil
	}
	if b.listener != nil {
		_ = b.listener.Close()
		b.listener = nil
	}
	slog.Info("otlp-grpc-bridge: stopped")
	return nil
}

func (b *otlpGRPCBridge) Healthy() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.server != nil && b.listener != nil
}

func (b *otlpGRPCBridge) HTTPHandler() http.Handler { return nil }

// forward marshals req as protobuf and POSTs it to the backend for signal.
// It POSTs to the direct Victoria insert URL (loopback), not to a vendor.
func (b *otlpGRPCBridge) forward(ctx context.Context, signal string, req proto.Message) error {
	url, ok := b.backend[signal]
	if !ok {
		return status.Errorf(codes.Unimplemented, "no backend for %s", signal)
	}
	if !strings.HasPrefix(url, "http://127.0.0.1:") && !strings.HasPrefix(url, "http://localhost:") {
		return status.Errorf(codes.Internal, "refusing non-loopback forward %q", url)
	}
	data, err := proto.Marshal(req)
	if err != nil {
		return status.Errorf(codes.Internal, "marshal %s: %v", signal, err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(data))
	if err != nil {
		return status.Errorf(codes.Internal, "create request %s: %v", signal, err)
	}
	httpReq.Header.Set("Content-Type", "application/x-protobuf")
	resp, err := b.client.Do(httpReq)
	if err != nil {
		return status.Errorf(codes.Unavailable, "forward %s to %s: %v", signal, url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return status.Errorf(codes.Unavailable, "forward %s to %s: status %d", signal, url, resp.StatusCode)
	}
	return nil
}

type grpcTraceServer struct {
	colltrace.UnimplementedTraceServiceServer
	bridge *otlpGRPCBridge
}

func (s *grpcTraceServer) Export(ctx context.Context, req *colltrace.ExportTraceServiceRequest) (*colltrace.ExportTraceServiceResponse, error) {
	if err := s.bridge.forward(ctx, "/v1/traces", req); err != nil {
		return nil, err
	}
	return &colltrace.ExportTraceServiceResponse{}, nil
}

type grpcLogsServer struct {
	collog.UnimplementedLogsServiceServer
	bridge *otlpGRPCBridge
}

func (s *grpcLogsServer) Export(ctx context.Context, req *collog.ExportLogsServiceRequest) (*collog.ExportLogsServiceResponse, error) {
	if err := s.bridge.forward(ctx, "/v1/logs", req); err != nil {
		return nil, err
	}
	return &collog.ExportLogsServiceResponse{}, nil
}

type grpcMetricsServer struct {
	collmetric.UnimplementedMetricsServiceServer
	bridge *otlpGRPCBridge
}

func (s *grpcMetricsServer) Export(ctx context.Context, req *collmetric.ExportMetricsServiceRequest) (*collmetric.ExportMetricsServiceResponse, error) {
	if err := s.bridge.forward(ctx, "/v1/metrics", req); err != nil {
		return nil, err
	}
	return &collmetric.ExportMetricsServiceResponse{}, nil
}
