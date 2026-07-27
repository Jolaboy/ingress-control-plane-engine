// Package xds — resources.go
// Translates a slice of RouteSpec (derived from IngressRoute CRs) into
// a complete, consistent Envoy xDS v3 snapshot containing:
//   - Listeners  (LDS) — one shared HTTP listener on port 10000
//   - RouteConfigurations (RDS) — one virtual host with all routes
//   - Clusters   (CDS) — one STRICT_DNS cluster per unique upstream service
//   - ClusterLoadAssignments (EDS) — embedded inside each cluster
//
// The ext_authz HTTP filter is injected into the listener's filter chain
// for every route that has AuthRequired=true. Routes without auth bypass
// the filter via per-route filter config override.
package xds

import (
	"fmt"
	"time"

	cluster "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpoint "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listener "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	route "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	extauthzv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/ext_authz/v3"
	routerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/router/v3"
	hcm "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	upstreamhttp "github.com/envoyproxy/go-control-plane/envoy/extensions/upstreams/http/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/envoyproxy/go-control-plane/pkg/wellknown"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const (
	// listenerName is the single shared Envoy listener.
	listenerName = "ingress-listener"
	// listenerPort is the port Envoy binds on inside its container.
	listenerPort = 10000
	// routeConfigName is the shared RDS route configuration name.
	routeConfigName = "ingress-routes"
	// virtualHostName is the virtual host catching all incoming traffic.
	virtualHostName = "local_service"
	// opaClusterName is the cluster pointing at the OPA ext_authz sidecar.
	opaClusterName = "opa-ext-authz"
	// opaAddress is the in-cluster DNS name of the OPA sidecar.
	opaAddress = "127.0.0.1"
	// opaPort is the gRPC port OPA listens on for ext_authz requests.
	opaPort = 9191
)

// RouteSpec is the canonical in-memory representation of one IngressRoute CR.
// The controller reconciler populates this struct and passes it to UpdateSnapshot.
type RouteSpec struct {
	// Name is the Kubernetes resource name (used as the Envoy route name).
	Name string
	// Namespace is the Kubernetes namespace the route lives in.
	Namespace string
	// Prefix is the URL path prefix to match (e.g. /api/v1/orders).
	Prefix string
	// ClusterName is the Envoy upstream cluster (derived from ServiceName).
	ClusterName string
	// ServiceName is the Kubernetes Service name.
	ServiceName string
	// ServicePort is the port on the Kubernetes Service.
	ServicePort uint32
	// Namespace used to build the in-cluster DNS FQDN.
	ServiceNamespace string
	// AuthRequired controls whether the ext_authz filter is active for this route.
	AuthRequired bool
	// TimeoutMs is the total route timeout in milliseconds (0 = disabled).
	TimeoutMs uint32
	// NumRetries is the number of retries Envoy will attempt.
	NumRetries uint32
	// PerTryTimeoutMs is the per-attempt timeout in milliseconds.
	PerTryTimeoutMs uint32
}

// clusterName produces a deterministic Envoy cluster name from a service reference.
func clusterNameFor(svc, ns string) string {
	return fmt.Sprintf("%s.%s", svc, ns)
}

// BuildSnapshot constructs a complete, valid Envoy cache.Snapshot from routes.
func BuildSnapshot(version string, routes []RouteSpec) (*cache.Snapshot, error) {
	clusters := buildClusters(routes)
	listeners, err := buildListeners(routes)
	if err != nil {
		return nil, fmt.Errorf("resources: build listeners: %w", err)
	}
	routeConfig := buildRouteConfig(routes)

	snap, err := cache.NewSnapshot(version,
		map[resource.Type][]types.Resource{
			resource.ClusterType:  clusters,
			resource.ListenerType: listeners,
			resource.RouteType:    {routeConfig},
			resource.EndpointType: {},
		},
	)
	if err != nil {
		return nil, fmt.Errorf("resources: new snapshot: %w", err)
	}
	return snap, nil
}

// ---------------------------------------------------------------------------
// CDS — Clusters
// ---------------------------------------------------------------------------

func buildClusters(routes []RouteSpec) []types.Resource {
	seen := map[string]bool{}
	var clusters []types.Resource

	// Always add the OPA ext_authz cluster
	clusters = append(clusters, opaCluster())

	for _, r := range routes {
		name := r.ClusterName
		if seen[name] {
			continue
		}
		seen[name] = true

		fqdn := fmt.Sprintf("%s.%s.svc.cluster.local", r.ServiceName, r.ServiceNamespace)
		c := &cluster.Cluster{
			Name:                 name,
			ConnectTimeout:       durationpb.New(5 * time.Second),
			ClusterDiscoveryType: &cluster.Cluster_Type{Type: cluster.Cluster_STRICT_DNS},
			DnsLookupFamily:      cluster.Cluster_V4_ONLY,
			LbPolicy:             cluster.Cluster_ROUND_ROBIN,
			// Circuit breaker — trip after 1024 pending requests
			CircuitBreakers: &cluster.CircuitBreakers{
				Thresholds: []*cluster.CircuitBreakers_Thresholds{
					{
						Priority:           core.RoutingPriority_DEFAULT,
						MaxPendingRequests: wrapperspb.UInt32(1024),
						MaxRequests:        wrapperspb.UInt32(1024),
					},
				},
			},
			LoadAssignment: &endpoint.ClusterLoadAssignment{
				ClusterName: name,
				Endpoints: []*endpoint.LocalityLbEndpoints{
					{
						LbEndpoints: []*endpoint.LbEndpoint{
							{
								HostIdentifier: &endpoint.LbEndpoint_Endpoint{
									Endpoint: &endpoint.Endpoint{
										Address: &core.Address{
											Address: &core.Address_SocketAddress{
												SocketAddress: &core.SocketAddress{
													Protocol: core.SocketAddress_TCP,
													Address:  fqdn,
													PortSpecifier: &core.SocketAddress_PortValue{
														PortValue: r.ServicePort,
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		}
		clusters = append(clusters, c)
	}
	return clusters
}

// opaCluster returns the static cluster pointing at the OPA ext_authz sidecar.
func opaCluster() *cluster.Cluster {
	return &cluster.Cluster{
		Name:                 opaClusterName,
		ConnectTimeout:       durationpb.New(1 * time.Second),
		ClusterDiscoveryType: &cluster.Cluster_Type{Type: cluster.Cluster_STATIC},
		// Use typed_extension_protocol_options to enable HTTP/2 for gRPC.
		// The legacy Http2ProtocolOptions field is deprecated in Envoy v1.28+.
		TypedExtensionProtocolOptions: map[string]*anypb.Any{
			"envoy.extensions.upstreams.http.v3.HttpProtocolOptions": mustAny(
				&upstreamhttp.HttpProtocolOptions{
					UpstreamProtocolOptions: &upstreamhttp.HttpProtocolOptions_ExplicitHttpConfig_{
						ExplicitHttpConfig: &upstreamhttp.HttpProtocolOptions_ExplicitHttpConfig{
							ProtocolConfig: &upstreamhttp.HttpProtocolOptions_ExplicitHttpConfig_Http2ProtocolOptions{
								Http2ProtocolOptions: &core.Http2ProtocolOptions{},
							},
						},
					},
				},
			),
		},
		LoadAssignment: &endpoint.ClusterLoadAssignment{
			ClusterName: opaClusterName,
			Endpoints: []*endpoint.LocalityLbEndpoints{
				{
					LbEndpoints: []*endpoint.LbEndpoint{
						{
							HostIdentifier: &endpoint.LbEndpoint_Endpoint{
								Endpoint: &endpoint.Endpoint{
									Address: &core.Address{
										Address: &core.Address_SocketAddress{
											SocketAddress: &core.SocketAddress{
												Protocol: core.SocketAddress_TCP,
												Address:  opaAddress,
												PortSpecifier: &core.SocketAddress_PortValue{
													PortValue: opaPort,
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

// ---------------------------------------------------------------------------
// LDS — Listeners
// ---------------------------------------------------------------------------

func buildListeners(routes []RouteSpec) ([]types.Resource, error) {
	// ext_authz filter — applied at the listener level; routes can disable it
	extAuthzFilter, err := buildExtAuthzFilter()
	if err != nil {
		return nil, err
	}

	hcmConfig := &hcm.HttpConnectionManager{
		StatPrefix: "ingress_http",
		RouteSpecifier: &hcm.HttpConnectionManager_Rds{
			Rds: &hcm.Rds{
				ConfigSource: &core.ConfigSource{
					ResourceApiVersion: core.ApiVersion_V3,
					ConfigSourceSpecifier: &core.ConfigSource_ApiConfigSource{
						ApiConfigSource: &core.ApiConfigSource{
							ApiType:             core.ApiConfigSource_GRPC,
							TransportApiVersion: core.ApiVersion_V3,
							GrpcServices: []*core.GrpcService{
								{
									TargetSpecifier: &core.GrpcService_EnvoyGrpc_{
										EnvoyGrpc: &core.GrpcService_EnvoyGrpc{
											ClusterName: "xds_cluster",
										},
									},
								},
							},
						},
					},
				},
				RouteConfigName: routeConfigName,
			},
		},
		HttpFilters: []*hcm.HttpFilter{
			extAuthzFilter,
			buildRouterFilter(),
		},
		// HTTP/2 upgrade support
		UpgradeConfigs: []*hcm.HttpConnectionManager_UpgradeConfig{
			{UpgradeType: "websocket"},
		},
		// Forward client IP in X-Forwarded-For
		UseRemoteAddress: wrapperspb.Bool(true),
	}

	hcmAny, err := anypb.New(hcmConfig)
	if err != nil {
		return nil, fmt.Errorf("resources: marshal HCM: %w", err)
	}

	l := &listener.Listener{
		Name: listenerName,
		Address: &core.Address{
			Address: &core.Address_SocketAddress{
				SocketAddress: &core.SocketAddress{
					Protocol: core.SocketAddress_TCP,
					Address:  "0.0.0.0",
					PortSpecifier: &core.SocketAddress_PortValue{
						PortValue: listenerPort,
					},
				},
			},
		},
		FilterChains: []*listener.FilterChain{
			{
				Filters: []*listener.Filter{
					{
						Name: wellknown.HTTPConnectionManager,
						ConfigType: &listener.Filter_TypedConfig{
							TypedConfig: hcmAny,
						},
					},
				},
			},
		},
	}

	return []types.Resource{l}, nil
}

// buildExtAuthzFilter constructs the ext_authz HCM HttpFilter pointing at OPA.
func buildExtAuthzFilter() (*hcm.HttpFilter, error) {
	extAuthz := &extauthzv3.ExtAuthz{
		Services: &extauthzv3.ExtAuthz_GrpcService{
			GrpcService: &core.GrpcService{
				TargetSpecifier: &core.GrpcService_EnvoyGrpc_{
					EnvoyGrpc: &core.GrpcService_EnvoyGrpc{
						ClusterName: opaClusterName,
					},
				},
				Timeout: durationpb.New(200 * time.Millisecond),
			},
		},
		TransportApiVersion:    core.ApiVersion_V3,
		IncludePeerCertificate: true,
		// Allow requests through if OPA is unavailable (fail-open).
		// Set to false for strict fail-closed behaviour.
		FailureModeAllow: false,
	}

	extAuthzAny, err := anypb.New(extAuthz)
	if err != nil {
		return nil, fmt.Errorf("resources: marshal ext_authz: %w", err)
	}

	return &hcm.HttpFilter{
		Name: wellknown.HTTPExternalAuthorization,
		ConfigType: &hcm.HttpFilter_TypedConfig{
			TypedConfig: extAuthzAny,
		},
	}, nil
}

// accessLogConfig is reserved for a future stdout/OTLP access log implementation.
// Envoy defaults to no access log when this field is omitted.

// buildRouterFilter returns the HCM Router filter with a fully-typed config.
// Envoy v1.28+ requires TypedConfig on the router filter; a bare name-only
// filter (empty type URL) causes the listener to be rejected at xDS apply time.
func buildRouterFilter() *hcm.HttpFilter {
	routerAny, err := anypb.New(&routerv3.Router{})
	if err != nil {
		return &hcm.HttpFilter{Name: wellknown.Router}
	}
	return &hcm.HttpFilter{
		Name: wellknown.Router,
		ConfigType: &hcm.HttpFilter_TypedConfig{
			TypedConfig: routerAny,
		},
	}
}

func buildRouteConfig(routes []RouteSpec) *route.RouteConfiguration {
	var envoyRoutes []*route.Route

	for _, r := range routes {
		envoyRoute := buildRoute(r)
		envoyRoutes = append(envoyRoutes, envoyRoute)
	}

	return &route.RouteConfiguration{
		Name: routeConfigName,
		VirtualHosts: []*route.VirtualHost{
			{
				Name:    virtualHostName,
				Domains: []string{"*"},
				Routes:  envoyRoutes,
			},
		},
	}
}

// buildRoute translates one RouteSpec into an Envoy route.Route.
func buildRoute(r RouteSpec) *route.Route {
	var retryPolicy *route.RetryPolicy
	if r.NumRetries > 0 {
		retryPolicy = &route.RetryPolicy{
			RetryOn:    "connect-failure,reset,5xx",
			NumRetries: wrapperspb.UInt32(r.NumRetries),
		}
		if r.PerTryTimeoutMs > 0 {
			retryPolicy.PerTryTimeout = durationpb.New(time.Duration(r.PerTryTimeoutMs) * time.Millisecond)
		}
	}

	var timeout *durationpb.Duration
	if r.TimeoutMs > 0 {
		timeout = durationpb.New(time.Duration(r.TimeoutMs) * time.Millisecond)
	}

	routeAction := &route.RouteAction{
		ClusterSpecifier: &route.RouteAction_Cluster{
			Cluster: r.ClusterName,
		},
		Timeout:     timeout,
		RetryPolicy: retryPolicy,
	}

	envoyRoute := &route.Route{
		Name: r.Name,
		Match: &route.RouteMatch{
			PathSpecifier: &route.RouteMatch_Prefix{
				Prefix: r.Prefix,
			},
		},
		Action: &route.Route_Route{
			Route: routeAction,
		},
	}

	// If auth is NOT required, disable ext_authz for this specific route
	// via per-route typed filter config override.
	if !r.AuthRequired {
		disabled := &extauthzv3.ExtAuthzPerRoute{
			Override: &extauthzv3.ExtAuthzPerRoute_Disabled{
				Disabled: true,
			},
		}
		disabledAny, err := anypb.New(disabled)
		if err == nil {
			envoyRoute.TypedPerFilterConfig = map[string]*anypb.Any{
				wellknown.HTTPExternalAuthorization: disabledAny,
			}
		}
	}

	return envoyRoute
}

// mustAny marshals a proto message into an anypb.Any, panicking on error.
// Used only in package-level cluster construction where errors are programmer bugs.
func mustAny(m proto.Message) *anypb.Any {
	a, err := anypb.New(m)
	if err != nil {
		panic(fmt.Sprintf("xds: mustAny marshal failed: %v", err))
	}
	return a
}
