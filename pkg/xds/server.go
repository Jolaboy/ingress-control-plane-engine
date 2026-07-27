// Package xds implements an Envoy xDS v3 gRPC management server.
// It maintains a SnapshotCache keyed by Envoy node-ID and exposes
// an UpdateSnapshot method that the controller reconciler calls
// whenever IngressRoute resources change in Kubernetes.
package xds

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"

	clusterservice "github.com/envoyproxy/go-control-plane/envoy/service/cluster/v3"
	discoverygrpc "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	endpointservice "github.com/envoyproxy/go-control-plane/envoy/service/endpoint/v3"
	listenerservice "github.com/envoyproxy/go-control-plane/envoy/service/listener/v3"
	routeservice "github.com/envoyproxy/go-control-plane/envoy/service/route/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"

	"time"
)

const (
	// DefaultNodeID is the Envoy node ID this server manages.
	// In production this should match the node.id in envoy-bootstrap.yaml.
	DefaultNodeID = "envoy-ingress-node-1"
)

// Server wraps an Envoy xDS cache and gRPC server.
type Server struct {
	cache   cache.SnapshotCache
	grpcSrv *grpc.Server
	mu      sync.Mutex
	version uint64
	logger  *slog.Logger
}

// NewServer creates an initialised xDS Server but does not start it yet.
func NewServer(logger *slog.Logger) *Server {
	snapshotCache := cache.NewSnapshotCache(
		false,          // ads — false means per-resource-type watches are independent
		cache.IDHash{}, // hash function for node IDs
		nil,            // logger (nil → silent; pass a cache.Logger adapter to enable)
	)

	xdsSrv := server.NewServer(context.Background(), snapshotCache, nil)

	grpcSrv := grpc.NewServer(
		grpc.MaxConcurrentStreams(1000),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    30 * time.Second,
			Timeout: 10 * time.Second,
		}),
	)

	// Register all xDS discovery services
	discoverygrpc.RegisterAggregatedDiscoveryServiceServer(grpcSrv, xdsSrv)
	clusterservice.RegisterClusterDiscoveryServiceServer(grpcSrv, xdsSrv)
	endpointservice.RegisterEndpointDiscoveryServiceServer(grpcSrv, xdsSrv)
	listenerservice.RegisterListenerDiscoveryServiceServer(grpcSrv, xdsSrv)
	routeservice.RegisterRouteDiscoveryServiceServer(grpcSrv, xdsSrv)

	// Enable server reflection so grpc_cli / grpcurl work out of the box
	reflection.Register(grpcSrv)

	return &Server{
		cache:   snapshotCache,
		grpcSrv: grpcSrv,
		logger:  logger,
	}
}

// Start binds to addr (e.g. ":18000") and begins serving xDS requests.
// It blocks until ctx is cancelled or an unrecoverable error occurs.
func (s *Server) Start(ctx context.Context, addr string) error {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("xds: failed to listen on %s: %w", addr, err)
	}

	s.logger.Info("xDS gRPC server listening", "addr", addr)

	errCh := make(chan error, 1)
	go func() {
		if err := s.grpcSrv.Serve(lis); err != nil {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		s.logger.Info("xDS server shutting down gracefully")
		s.grpcSrv.GracefulStop()
		return nil
	case err := <-errCh:
		return fmt.Errorf("xds: gRPC serve error: %w", err)
	}
}

// UpdateSnapshot atomically replaces the xDS snapshot for the given nodeID
// with a new one built from the supplied RouteSpec slice.
// It increments an internal monotonic version counter so Envoy always
// detects the change and re-fetches configuration.
func (s *Server) UpdateSnapshot(nodeID string, routes []RouteSpec) error {
	s.mu.Lock()
	s.version++
	version := fmt.Sprintf("v%d", s.version)
	s.mu.Unlock()

	snap, err := BuildSnapshot(version, routes)
	if err != nil {
		return fmt.Errorf("xds: build snapshot: %w", err)
	}

	if err := s.cache.SetSnapshot(context.Background(), nodeID, snap); err != nil {
		return fmt.Errorf("xds: set snapshot for node %q: %w", nodeID, err)
	}

	s.logger.Info("xDS snapshot updated",
		"nodeID", nodeID,
		"version", version,
		"routes", len(routes),
	)
	return nil
}

// SnapshotVersion returns the current snapshot version string for a node.
func (s *Server) SnapshotVersion(nodeID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fmt.Sprintf("v%d", s.version)
}

// ---------------------------------------------------------------------------
// Internal: xDS callback logger adapter (unused — kept for future extension)
// ---------------------------------------------------------------------------
