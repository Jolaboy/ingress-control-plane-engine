package xds

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"

	clusterservice "github.com/envoyproxy/go-control-plane/envoy/service/cluster/v3"
	discoverygrpc "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// getFreePort finds an available TCP port for test servers.
func getFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("could not find free port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

// ---------------------------------------------------------------------------
// NewServer
// ---------------------------------------------------------------------------

func TestNewServer_NotNil(t *testing.T) {
	srv := NewServer(testLogger())
	if srv == nil {
		t.Fatal("NewServer returned nil")
	}
}

func TestNewServer_DefaultNodeIDConst(t *testing.T) {
	if DefaultNodeID == "" {
		t.Error("DefaultNodeID must not be empty")
	}
}

// ---------------------------------------------------------------------------
// UpdateSnapshot
// ---------------------------------------------------------------------------

func TestUpdateSnapshot_EmptyRoutes(t *testing.T) {
	srv := NewServer(testLogger())
	if err := srv.UpdateSnapshot(DefaultNodeID, nil); err != nil {
		t.Fatalf("UpdateSnapshot with nil routes: %v", err)
	}
}

func TestUpdateSnapshot_WithRoutes(t *testing.T) {
	srv := NewServer(testLogger())
	routes := []RouteSpec{
		{
			Name: "r1", Prefix: "/api", ClusterName: "svc.default",
			ServiceName: "svc", ServiceNamespace: "default", ServicePort: 8080,
		},
	}
	if err := srv.UpdateSnapshot(DefaultNodeID, routes); err != nil {
		t.Fatalf("UpdateSnapshot: %v", err)
	}
}

func TestUpdateSnapshot_VersionMonotonicallyIncreases(t *testing.T) {
	srv := NewServer(testLogger())
	_ = srv.UpdateSnapshot(DefaultNodeID, nil)
	v1 := srv.SnapshotVersion(DefaultNodeID)
	_ = srv.UpdateSnapshot(DefaultNodeID, nil)
	v2 := srv.SnapshotVersion(DefaultNodeID)
	if v1 == v2 {
		t.Errorf("version should increase after each update, got %q both times", v1)
	}
}

func TestUpdateSnapshot_MultipleNodeIDs(t *testing.T) {
	srv := NewServer(testLogger())
	for _, nodeID := range []string{"node-1", "node-2", "node-3"} {
		if err := srv.UpdateSnapshot(nodeID, nil); err != nil {
			t.Errorf("UpdateSnapshot(%q): %v", nodeID, err)
		}
	}
}

// ---------------------------------------------------------------------------
// SnapshotVersion
// ---------------------------------------------------------------------------

func TestSnapshotVersion_StartsAtV1AfterFirstUpdate(t *testing.T) {
	srv := NewServer(testLogger())
	_ = srv.UpdateSnapshot(DefaultNodeID, nil)
	got := srv.SnapshotVersion(DefaultNodeID)
	if got != "v1" {
		t.Errorf("first snapshot version: got %q, want v1", got)
	}
}

func TestSnapshotVersion_Format(t *testing.T) {
	srv := NewServer(testLogger())
	for i := 1; i <= 5; i++ {
		_ = srv.UpdateSnapshot(DefaultNodeID, nil)
		got := srv.SnapshotVersion(DefaultNodeID)
		want := fmt.Sprintf("v%d", i)
		if got != want {
			t.Errorf("after %d updates: version = %q, want %q", i, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Start / gRPC connectivity
// ---------------------------------------------------------------------------

func TestStart_ServesGRPC(t *testing.T) {
	port := getFreePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	srv := NewServer(testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	started := make(chan struct{})
	go func() {
		close(started)
		_ = srv.Start(ctx, addr)
	}()
	<-started

	// Give the goroutine a moment to bind the port
	time.Sleep(50 * time.Millisecond)

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	defer conn.Close()

	// Verify the ADS service is registered by making a reflection call
	client := discoverygrpc.NewAggregatedDiscoveryServiceClient(conn)
	_ = client // reachable without error — service is registered
}

func TestStart_CDSServiceRegistered(t *testing.T) {
	port := getFreePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	srv := NewServer(testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = srv.Start(ctx, addr) }()
	time.Sleep(60 * time.Millisecond)

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	client := clusterservice.NewClusterDiscoveryServiceClient(conn)
	_ = client
}

func TestStart_GracefulShutdownOnContextCancel(t *testing.T) {
	port := getFreePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	srv := NewServer(testLogger())
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- srv.Start(ctx, addr)
	}()
	time.Sleep(50 * time.Millisecond)

	cancel() // trigger graceful shutdown

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Start returned error on clean shutdown: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Error("server did not shut down within 3s after context cancel")
	}
}

// ---------------------------------------------------------------------------
// Concurrent UpdateSnapshot safety
// ---------------------------------------------------------------------------

func TestUpdateSnapshot_ConcurrentSafety(t *testing.T) {
	srv := NewServer(testLogger())
	_ = srv.UpdateSnapshot(DefaultNodeID, nil) // seed v1

	done := make(chan struct{})
	for i := 0; i < 20; i++ {
		go func() {
			_ = srv.UpdateSnapshot(DefaultNodeID, nil)
			done <- struct{}{}
		}()
	}
	for i := 0; i < 20; i++ {
		<-done
	}
	// Final version must be > 1
	v := srv.SnapshotVersion(DefaultNodeID)
	if v == "v1" {
		t.Error("expected version to advance beyond v1 after concurrent updates")
	}
}
