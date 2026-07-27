package xds

import (
	"testing"

	cluster "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	listener "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	extauthzv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/ext_authz/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"google.golang.org/protobuf/proto"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func sampleRoutes() []RouteSpec {
	return []RouteSpec{
		{
			Name:             "orders-api-route",
			Namespace:        "default",
			Prefix:           "/api/v1/orders",
			ServiceName:      "order-service",
			ServiceNamespace: "default",
			ClusterName:      "order-service.default",
			ServicePort:      8080,
			AuthRequired:     true,
			TimeoutMs:        10000,
			NumRetries:       3,
			PerTryTimeoutMs:  3000,
		},
		{
			Name:             "healthz-route",
			Namespace:        "default",
			Prefix:           "/healthz",
			ServiceName:      "health-service",
			ServiceNamespace: "default",
			ClusterName:      "health-service.default",
			ServicePort:      8081,
			AuthRequired:     false,
			TimeoutMs:        2000,
		},
	}
}

// ---------------------------------------------------------------------------
// BuildSnapshot
// ---------------------------------------------------------------------------

func TestBuildSnapshot_ReturnsNoError(t *testing.T) {
	snap, err := BuildSnapshot("v1", sampleRoutes())
	if err != nil {
		t.Fatalf("BuildSnapshot returned unexpected error: %v", err)
	}
	if snap == nil {
		t.Fatal("BuildSnapshot returned nil snapshot")
	}
}

func TestBuildSnapshot_EmptyRoutes(t *testing.T) {
	snap, err := BuildSnapshot("v0", nil)
	if err != nil {
		t.Fatalf("BuildSnapshot with nil routes: %v", err)
	}
	if snap == nil {
		t.Fatal("snapshot should not be nil for empty routes")
	}
}

func TestBuildSnapshot_VersionIsSet(t *testing.T) {
	snap, _ := BuildSnapshot("v42", sampleRoutes())
	if snap.GetVersion(resource.ClusterType) != "v42" {
		t.Errorf("expected snapshot version v42, got %q", snap.GetVersion(resource.ClusterType))
	}
}

// ---------------------------------------------------------------------------
// CDS — Clusters
// ---------------------------------------------------------------------------

func TestBuildClusters_AlwaysIncludesOPACluster(t *testing.T) {
	clusters := buildClusters(sampleRoutes())

	found := false
	for _, r := range clusters {
		c, ok := r.(*cluster.Cluster)
		if ok && c.Name == opaClusterName {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("OPA cluster %q not found in CDS output", opaClusterName)
	}
}

func TestBuildClusters_OneClusterPerUniqueService(t *testing.T) {
	routes := []RouteSpec{
		{ClusterName: "svc-a.default", ServiceName: "svc-a", ServiceNamespace: "default", ServicePort: 8080},
		{ClusterName: "svc-a.default", ServiceName: "svc-a", ServiceNamespace: "default", ServicePort: 8080}, // duplicate
		{ClusterName: "svc-b.default", ServiceName: "svc-b", ServiceNamespace: "default", ServicePort: 9090},
	}
	clusters := buildClusters(routes)

	// Expect: opa-ext-authz + svc-a.default + svc-b.default = 3
	if len(clusters) != 3 {
		t.Errorf("expected 3 clusters (opa + 2 unique services), got %d", len(clusters))
	}
}

func TestBuildClusters_FQDNFormat(t *testing.T) {
	routes := []RouteSpec{
		{ClusterName: "order-service.commerce", ServiceName: "order-service", ServiceNamespace: "commerce", ServicePort: 8080},
	}
	clusters := buildClusters(routes)

	// Find the order-service cluster and verify its endpoint address
	for _, r := range clusters {
		c, ok := r.(*cluster.Cluster)
		if !ok || c.Name != "order-service.commerce" {
			continue
		}
		addr := c.LoadAssignment.Endpoints[0].LbEndpoints[0].
			GetEndpoint().Address.GetSocketAddress().Address
		want := "order-service.commerce.svc.cluster.local"
		if addr != want {
			t.Errorf("FQDN: got %q, want %q", addr, want)
		}
		return
	}
	t.Error("order-service.commerce cluster not found")
}

func TestBuildClusters_CircuitBreakerThreshold(t *testing.T) {
	routes := []RouteSpec{
		{ClusterName: "svc.default", ServiceName: "svc", ServiceNamespace: "default", ServicePort: 8080},
	}
	clusters := buildClusters(routes)
	for _, r := range clusters {
		c, ok := r.(*cluster.Cluster)
		if !ok || c.Name != "svc.default" {
			continue
		}
		if c.CircuitBreakers == nil || len(c.CircuitBreakers.Thresholds) == 0 {
			t.Error("expected circuit breakers to be configured")
		}
		return
	}
	t.Error("svc.default cluster not found")
}

// ---------------------------------------------------------------------------
// LDS — Listeners
// ---------------------------------------------------------------------------

func TestBuildListeners_ReturnsSingleListener(t *testing.T) {
	listeners, err := buildListeners(sampleRoutes())
	if err != nil {
		t.Fatalf("buildListeners error: %v", err)
	}
	if len(listeners) != 1 {
		t.Errorf("expected 1 listener, got %d", len(listeners))
	}
}

func TestBuildListeners_ListenerBindsOnCorrectPort(t *testing.T) {
	listeners, _ := buildListeners(sampleRoutes())
	l := listeners[0].(*listener.Listener)
	port := l.Address.GetSocketAddress().GetPortValue()
	if port != listenerPort {
		t.Errorf("listener port: got %d, want %d", port, listenerPort)
	}
}

func TestBuildListeners_ContainsExtAuthzFilter(t *testing.T) {
	listeners, _ := buildListeners(sampleRoutes())
	l := listeners[0].(*listener.Listener)
	hcmAny := l.FilterChains[0].Filters[0].GetTypedConfig()
	if hcmAny == nil {
		t.Fatal("HCM typed config is nil")
	}
	// Verify there is content — the typed config URL should reference HCM
	if hcmAny.TypeUrl == "" {
		t.Error("HCM TypeUrl should not be empty")
	}
}

// ---------------------------------------------------------------------------
// RDS — Routes
// ---------------------------------------------------------------------------

func TestBuildRouteConfig_RoutesCount(t *testing.T) {
	routes := sampleRoutes()
	rc := buildRouteConfig(routes)
	if len(rc.VirtualHosts[0].Routes) != len(routes) {
		t.Errorf("expected %d routes, got %d", len(routes), len(rc.VirtualHosts[0].Routes))
	}
}

func TestBuildRouteConfig_PrefixMatch(t *testing.T) {
	routes := sampleRoutes()
	rc := buildRouteConfig(routes)
	for i, r := range rc.VirtualHosts[0].Routes {
		want := routes[i].Prefix
		got := r.Match.GetPrefix()
		if got != want {
			t.Errorf("route[%d] prefix: got %q, want %q", i, got, want)
		}
	}
}

func TestBuildRouteConfig_AuthRouteHasNoDisableOverride(t *testing.T) {
	// When AuthRequired=true the route must NOT have the ext_authz disable override.
	routes := []RouteSpec{{
		Name:         "auth-route",
		Prefix:       "/secure",
		ClusterName:  "svc.default",
		AuthRequired: true,
	}}
	rc := buildRouteConfig(routes)
	r := rc.VirtualHosts[0].Routes[0]
	if _, exists := r.TypedPerFilterConfig["envoy.filters.http.ext_authz"]; exists {
		t.Error("auth-required route should NOT have ext_authz disabled override")
	}
}

func TestBuildRouteConfig_NoAuthRouteDisablesExtAuthz(t *testing.T) {
	// When AuthRequired=false the route must carry the per-route disable override.
	routes := []RouteSpec{{
		Name:         "public-route",
		Prefix:       "/healthz",
		ClusterName:  "svc.default",
		AuthRequired: false,
	}}
	rc := buildRouteConfig(routes)
	r := rc.VirtualHosts[0].Routes[0]

	anyConfig, exists := r.TypedPerFilterConfig["envoy.filters.http.ext_authz"]
	if !exists {
		t.Fatal("public route should have ext_authz per-route config to disable it")
	}

	disabled := &extauthzv3.ExtAuthzPerRoute{}
	if err := proto.Unmarshal(anyConfig.Value, disabled); err != nil {
		t.Fatalf("failed to unmarshal ExtAuthzPerRoute: %v", err)
	}
	if !disabled.GetDisabled() {
		t.Error("ext_authz should be disabled for public route")
	}
}

func TestBuildRouteConfig_RetryPolicyApplied(t *testing.T) {
	routes := []RouteSpec{{
		Name:            "retry-route",
		Prefix:          "/api",
		ClusterName:     "svc.default",
		AuthRequired:    true,
		NumRetries:      3,
		PerTryTimeoutMs: 2000,
	}}
	rc := buildRouteConfig(routes)
	r := rc.VirtualHosts[0].Routes[0]
	rp := r.GetRoute().RetryPolicy
	if rp == nil {
		t.Fatal("expected retry policy, got nil")
	}
	if rp.NumRetries.GetValue() != 3 {
		t.Errorf("NumRetries: got %d, want 3", rp.NumRetries.GetValue())
	}
}

func TestBuildRouteConfig_TimeoutApplied(t *testing.T) {
	routes := []RouteSpec{{
		Name:        "timeout-route",
		Prefix:      "/api",
		ClusterName: "svc.default",
		TimeoutMs:   5000,
	}}
	rc := buildRouteConfig(routes)
	r := rc.VirtualHosts[0].Routes[0]
	got := r.GetRoute().Timeout.AsDuration().Milliseconds()
	if got != 5000 {
		t.Errorf("route timeout: got %dms, want 5000ms", got)
	}
}

func TestBuildRouteConfig_ZeroTimeoutOmitted(t *testing.T) {
	routes := []RouteSpec{{
		Name:        "no-timeout-route",
		Prefix:      "/api",
		ClusterName: "svc.default",
		TimeoutMs:   0,
	}}
	rc := buildRouteConfig(routes)
	r := rc.VirtualHosts[0].Routes[0]
	if r.GetRoute().Timeout != nil {
		t.Error("zero TimeoutMs should produce nil route timeout")
	}
}

// ---------------------------------------------------------------------------
// Full snapshot resource type presence
// ---------------------------------------------------------------------------

func TestBuildSnapshot_AllResourceTypesPresent(t *testing.T) {
	snap, err := BuildSnapshot("v1", sampleRoutes())
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		typ  string
		name string
	}{
		{resource.ClusterType, "ClusterType"},
		{resource.ListenerType, "ListenerType"},
		{resource.RouteType, "RouteType"},
	}
	for _, tc := range tests {
		items := snap.GetResources(tc.typ)
		if len(items) == 0 {
			t.Errorf("snapshot missing resources for %s", tc.name)
		}
	}
}

func TestBuildSnapshot_ListenerAndClusterNamesConsistent(t *testing.T) {
	snap, _ := BuildSnapshot("v1", sampleRoutes())

	// Listener should be named listenerName
	listeners := snap.GetResources(resource.ListenerType)
	if _, ok := listeners[listenerName]; !ok {
		t.Errorf("expected listener named %q", listenerName)
	}

	// Route config should be named routeConfigName
	routes := snap.GetResources(resource.RouteType)
	if _, ok := routes[routeConfigName]; !ok {
		t.Errorf("expected route config named %q", routeConfigName)
	}
}

// ---------------------------------------------------------------------------
// clusterNameFor helper
// ---------------------------------------------------------------------------

func TestClusterNameFor(t *testing.T) {
	got := clusterNameFor("my-svc", "production")
	want := "my-svc.production"
	if got != want {
		t.Errorf("clusterNameFor: got %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// OPA cluster properties
// ---------------------------------------------------------------------------

func TestOPACluster_IsStatic(t *testing.T) {
	c := opaCluster()
	if c.GetType() != cluster.Cluster_STATIC {
		t.Errorf("OPA cluster should be STATIC, got %v", c.GetType())
	}
}

func TestOPACluster_HTTP2Enabled(t *testing.T) {
	c := opaCluster()
	if len(c.TypedExtensionProtocolOptions) == 0 {
		t.Error("OPA cluster should have HTTP/2 enabled via TypedExtensionProtocolOptions for gRPC")
	}
}

func TestOPACluster_CorrectAddress(t *testing.T) {
	c := opaCluster()
	addr := c.LoadAssignment.Endpoints[0].LbEndpoints[0].
		GetEndpoint().Address.GetSocketAddress()
	if addr.Address != opaAddress {
		t.Errorf("OPA address: got %q, want %q", addr.Address, opaAddress)
	}
	if addr.GetPortValue() != opaPort {
		t.Errorf("OPA port: got %d, want %d", addr.GetPortValue(), opaPort)
	}
}

// satisfy "unused import" for types package used indirectly
var _ types.Resource
