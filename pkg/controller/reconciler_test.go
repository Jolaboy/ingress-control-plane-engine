package controller

import (
	"log/slog"
	"os"
	"testing"

	"github.com/Jolaboy/ingress-control-plane-engine/pkg/xds"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ---------------------------------------------------------------------------
// Fake xDS updater for testing without a real gRPC server
// ---------------------------------------------------------------------------

type fakeXDS struct {
	lastRoutes []xds.RouteSpec
	callCount  int
	version    string
}

func (f *fakeXDS) UpdateSnapshot(_ string, routes []xds.RouteSpec) error {
	f.lastRoutes = routes
	f.callCount++
	f.version = "v1"
	return nil
}

func (f *fakeXDS) SnapshotVersion(_ string) string { return f.version }

// ---------------------------------------------------------------------------
// buildRouteSpecs
// ---------------------------------------------------------------------------

func makeUnstructuredList(items []map[string]interface{}) *unstructured.UnstructuredList {
	list := &unstructured.UnstructuredList{}
	for _, item := range items {
		u := unstructured.Unstructured{}
		u.Object = item
		list.Items = append(list.Items, u)
	}
	return list
}

func newReconciler() *IngressRouteReconciler {
	return &IngressRouteReconciler{
		XDS:    &fakeXDS{},
		NodeID: "test-node",
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	}
}

func TestBuildRouteSpecs_ValidRoute(t *testing.T) {
	r := newReconciler()
	list := makeUnstructuredList([]map[string]interface{}{
		{
			"metadata": map[string]interface{}{"name": "orders", "namespace": "default"},
			"spec": map[string]interface{}{
				"prefix":       "/api/v1/orders",
				"serviceName":  "order-service",
				"servicePort":  int64(8080),
				"authRequired": true,
				"timeoutMs":    int64(10000),
				"retryPolicy": map[string]interface{}{
					"numRetries":      int64(3),
					"perTryTimeoutMs": int64(3000),
				},
			},
		},
	})

	routes, err := r.buildRouteSpecs(list)
	if err != nil {
		t.Fatalf("buildRouteSpecs: %v", err)
	}
	if len(routes) != 1 {
		t.Fatalf("expected 1 route, got %d", len(routes))
	}

	got := routes[0]
	if got.Name != "orders" {
		t.Errorf("Name: got %q, want orders", got.Name)
	}
	if got.Prefix != "/api/v1/orders" {
		t.Errorf("Prefix: got %q", got.Prefix)
	}
	if got.ServiceName != "order-service" {
		t.Errorf("ServiceName: got %q", got.ServiceName)
	}
	if got.ServicePort != 8080 {
		t.Errorf("ServicePort: got %d, want 8080", got.ServicePort)
	}
	if !got.AuthRequired {
		t.Error("AuthRequired should be true")
	}
	if got.TimeoutMs != 10000 {
		t.Errorf("TimeoutMs: got %d, want 10000", got.TimeoutMs)
	}
	if got.NumRetries != 3 {
		t.Errorf("NumRetries: got %d, want 3", got.NumRetries)
	}
	if got.PerTryTimeoutMs != 3000 {
		t.Errorf("PerTryTimeoutMs: got %d, want 3000", got.PerTryTimeoutMs)
	}
}

func TestBuildRouteSpecs_ClusterNameDerived(t *testing.T) {
	r := newReconciler()
	list := makeUnstructuredList([]map[string]interface{}{
		{
			"metadata": map[string]interface{}{"name": "r1", "namespace": "payments"},
			"spec": map[string]interface{}{
				"prefix":      "/pay",
				"serviceName": "pay-svc",
				"servicePort": int64(8080),
			},
		},
	})
	routes, _ := r.buildRouteSpecs(list)
	if routes[0].ClusterName != "pay-svc.payments" {
		t.Errorf("ClusterName: got %q, want pay-svc.payments", routes[0].ClusterName)
	}
}

func TestBuildRouteSpecs_SkipsRoutesMissingPrefix(t *testing.T) {
	r := newReconciler()
	list := makeUnstructuredList([]map[string]interface{}{
		{
			"metadata": map[string]interface{}{"name": "bad", "namespace": "default"},
			"spec": map[string]interface{}{
				"serviceName": "svc",
				"servicePort": int64(8080),
				// prefix intentionally missing
			},
		},
	})
	routes, err := r.buildRouteSpecs(list)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(routes) != 0 {
		t.Errorf("expected 0 routes for missing prefix, got %d", len(routes))
	}
}

func TestBuildRouteSpecs_SkipsRoutesMissingServiceName(t *testing.T) {
	r := newReconciler()
	list := makeUnstructuredList([]map[string]interface{}{
		{
			"metadata": map[string]interface{}{"name": "bad", "namespace": "default"},
			"spec": map[string]interface{}{
				"prefix":      "/api",
				"servicePort": int64(8080),
				// serviceName intentionally missing
			},
		},
	})
	routes, _ := r.buildRouteSpecs(list)
	if len(routes) != 0 {
		t.Errorf("expected 0 routes for missing serviceName, got %d", len(routes))
	}
}

func TestBuildRouteSpecs_SkipsMissingSpec(t *testing.T) {
	r := newReconciler()
	list := makeUnstructuredList([]map[string]interface{}{
		{
			"metadata": map[string]interface{}{"name": "no-spec", "namespace": "default"},
			// no spec key at all
		},
	})
	routes, _ := r.buildRouteSpecs(list)
	if len(routes) != 0 {
		t.Errorf("expected 0 routes for missing spec, got %d", len(routes))
	}
}

func TestBuildRouteSpecs_DefaultsNamespaceToDefault(t *testing.T) {
	r := newReconciler()
	list := makeUnstructuredList([]map[string]interface{}{
		{
			"metadata": map[string]interface{}{"name": "r1"}, // no namespace
			"spec": map[string]interface{}{
				"prefix":      "/x",
				"serviceName": "svc",
				"servicePort": int64(80),
			},
		},
	})
	routes, _ := r.buildRouteSpecs(list)
	if len(routes) == 0 {
		t.Fatal("expected 1 route")
	}
	if routes[0].Namespace != "default" {
		t.Errorf("Namespace: got %q, want default", routes[0].Namespace)
	}
}

func TestBuildRouteSpecs_MultipleRoutes(t *testing.T) {
	r := newReconciler()
	items := []map[string]interface{}{}
	for i := 0; i < 10; i++ {
		items = append(items, map[string]interface{}{
			"metadata": map[string]interface{}{"name": "r", "namespace": "default"},
			"spec": map[string]interface{}{
				"prefix":      "/api",
				"serviceName": "svc",
				"servicePort": int64(8080),
			},
		})
	}
	routes, _ := r.buildRouteSpecs(makeUnstructuredList(items))
	if len(routes) != 10 {
		t.Errorf("expected 10 routes, got %d", len(routes))
	}
}

// ---------------------------------------------------------------------------
// toUint32
// ---------------------------------------------------------------------------

func TestToUint32_Conversions(t *testing.T) {
	cases := []struct {
		in   interface{}
		want uint32
	}{
		{int64(8080), 8080},
		{float64(9090), 9090},
		{int(443), 443},
		{int32(80), 80},
		{uint32(8443), 8443},
		{nil, 0},
		{"not-a-number", 0},
	}
	for _, tc := range cases {
		got := toUint32(tc.in)
		if got != tc.want {
			t.Errorf("toUint32(%v) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// replaceOrAppendCondition
// ---------------------------------------------------------------------------

func TestReplaceOrAppendCondition_AppendsWhenEmpty(t *testing.T) {
	cond := map[string]interface{}{"type": "Ready", "status": "True"}
	result := replaceOrAppendCondition(nil, cond)
	if len(result) != 1 {
		t.Errorf("expected 1 condition, got %d", len(result))
	}
}

func TestReplaceOrAppendCondition_ReplacesExisting(t *testing.T) {
	existing := []interface{}{
		map[string]interface{}{"type": "Ready", "status": "False"},
		map[string]interface{}{"type": "Synced", "status": "True"},
	}
	newCond := map[string]interface{}{"type": "Ready", "status": "True"}
	result := replaceOrAppendCondition(existing, newCond)
	if len(result) != 2 {
		t.Errorf("should stay at 2 conditions, got %d", len(result))
	}
	m := result[0].(map[string]interface{})
	if m["status"] != "True" {
		t.Error("Ready condition should have been replaced with status=True")
	}
}

func TestReplaceOrAppendCondition_AppendsNewType(t *testing.T) {
	existing := []interface{}{
		map[string]interface{}{"type": "Ready", "status": "True"},
	}
	newCond := map[string]interface{}{"type": "Degraded", "status": "False"}
	result := replaceOrAppendCondition(existing, newCond)
	if len(result) != 2 {
		t.Errorf("expected 2 conditions after append, got %d", len(result))
	}
}
