// Package xds implements an Envoy xDS v3 gRPC management server.
// It maintains a SnapshotCache keyed by Envoy node-ID and exposes
// an UpdateSnapshot method that the controller reconciler calls
// whenever IngressRoute resources change in Kubernetes.
package xds

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"

	clusterservice "github.com/envoyproxy/go-control-plane/envoy/service/cluster/v3"
	discoverygrpc "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	endpointservice "github.com/envoyproxy/go-control-plane/envoy/service/endpoint/v3"
	listenerservice "github.com/envoyproxy/go-control-plane/envoy/service/listener/v3"
	routeservice "github.com/envoyproxy/go-control-plane/envoy/service/route/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"

	"time"
)

const (
	// DefaultNodeID is the Envoy node ID this server manages.
	// In production this should match the node.id in envoy-bootstrap.yaml.
	DefaultNodeID = "envoy-ingress-node-1"
)

// TLSConfig holds the file paths for the xDS server's mutual-TLS material.
// When supplied via WithMTLS, the gRPC server requires and verifies a client
// certificate (Envoy) signed by the configured CA. All files are re-read on
// every TLS handshake so cert-manager can rotate them without a pod restart.
type TLSConfig struct {
	// CertFile is the PEM-encoded xDS server certificate.
	CertFile string
	// KeyFile is the PEM-encoded xDS server private key.
	KeyFile string
	// CAFile is the PEM-encoded CA bundle used to verify Envoy client certs.
	CAFile string
}

// Option configures an xDS Server at construction time.
type Option func(*serverOptions)

type serverOptions struct {
	tls *TLSConfig
}

// WithMTLS enables mutual TLS on the xDS gRPC server using the supplied
// certificate, key, and client-CA files.
func WithMTLS(tc TLSConfig) Option {
	return func(o *serverOptions) { o.tls = &tc }
}

// Server wraps an Envoy xDS cache and gRPC server.
type Server struct {
	cache   cache.SnapshotCache
	grpcSrv *grpc.Server
	mu      sync.Mutex
	version uint64
	logger  *slog.Logger
}

// NewServer creates an initialised xDS Server but does not start it yet.
// Pass WithMTLS(...) to secure the xDS gRPC channel with mutual TLS.
func NewServer(logger *slog.Logger, opts ...Option) *Server {
	o := &serverOptions{}
	for _, opt := range opts {
		opt(o)
	}

	snapshotCache := cache.NewSnapshotCache(
		false,          // ads — false means per-resource-type watches are independent
		cache.IDHash{}, // hash function for node IDs
		nil,            // logger (nil → silent; pass a cache.Logger adapter to enable)
	)

	xdsSrv := server.NewServer(context.Background(), snapshotCache, nil)

	grpcOpts := []grpc.ServerOption{
		grpc.MaxConcurrentStreams(1000),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    30 * time.Second,
			Timeout: 10 * time.Second,
		}),
	}

	// Enable mutual TLS when configured. Credentials reload the key material
	// from disk on every handshake, so cert-manager rotation is transparent.
	if o.tls != nil {
		grpcOpts = append(grpcOpts, grpc.Creds(credentials.NewTLS(newServerTLSConfig(*o.tls))))
		logger.Info("xDS gRPC server secured with mutual TLS",
			"certFile", o.tls.CertFile,
			"caFile", o.tls.CAFile,
		)
	}

	grpcSrv := grpc.NewServer(grpcOpts...)

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

// newServerTLSConfig builds a *tls.Config that enforces mutual TLS and reloads
// the server keypair and client-CA bundle from disk on each handshake. This
// keeps long-lived Envoy connections valid across cert-manager rotations.
func newServerTLSConfig(tc TLSConfig) *tls.Config {
	loadCert := func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		cert, err := tls.LoadX509KeyPair(tc.CertFile, tc.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("xds: load server keypair: %w", err)
		}
		return &cert, nil
	}

	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		ClientAuth:     tls.RequireAndVerifyClientCert,
		GetCertificate: loadCert,
		// GetConfigForClient re-reads the client CA on every handshake so a
		// rotated CA bundle is picked up without restarting the controller.
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			caPEM, err := os.ReadFile(tc.CAFile)
			if err != nil {
				return nil, fmt.Errorf("xds: read client CA %s: %w", tc.CAFile, err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(caPEM) {
				return nil, fmt.Errorf("xds: no valid certificates in client CA %s", tc.CAFile)
			}
			return &tls.Config{
				MinVersion:     tls.VersionTLS12,
				ClientAuth:     tls.RequireAndVerifyClientCert,
				ClientCAs:      pool,
				GetCertificate: loadCert,
			}, nil
		},
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
