// Package controller implements a Kubernetes controller that watches
// IngressRoute custom resources and translates them into Envoy xDS snapshots.
//
// Architecture:
//
//	IngressRoute CR (Kubernetes API) ──► Reconciler ──► xDS Server (gRPC)
//	                                         │
//	                                    Lists ALL IngressRoutes
//	                                    in all namespaces, builds
//	                                    a complete RouteSpec slice,
//	                                    then calls xds.UpdateSnapshot.
//
// The reconciler uses controller-runtime which handles:
//   - Leader election (safe for multi-replica deployments)
//   - Exponential back-off on errors
//   - Work queue deduplication
//   - Graceful shutdown
package controller

import (
	"context"
	"fmt"
	"log/slog"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/Jolaboy/ingress-control-plane-engine/pkg/xds"
)

// IngressRouteGVR is the GroupVersionResource for IngressRoute CRs.
var IngressRouteGVR = schema.GroupVersionResource{
	Group:    "platform.internal",
	Version:  "v1alpha1",
	Resource: "ingressroutes",
}

// IngressRouteGVK is the GroupVersionKind for IngressRoute CRs.
var IngressRouteGVK = schema.GroupVersionKind{
	Group:   "platform.internal",
	Version: "v1alpha1",
	Kind:    "IngressRoute",
}

// XDSUpdater is the interface the reconciler uses to push xDS snapshots.
// This thin interface makes unit testing straightforward.
type XDSUpdater interface {
	UpdateSnapshot(nodeID string, routes []xds.RouteSpec) error
	SnapshotVersion(nodeID string) string
}

// IngressRouteReconciler reconciles IngressRoute objects.
type IngressRouteReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	XDS    XDSUpdater
	NodeID string
	Logger *slog.Logger
}

// +kubebuilder:rbac:groups=platform.internal,resources=ingressroutes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=platform.internal,resources=ingressroutes/status,verbs=get;update;patch

// Reconcile is called by controller-runtime whenever an IngressRoute is
// created, updated, or deleted. It re-lists all IngressRoutes across all
// namespaces and pushes a fresh xDS snapshot.
func (r *IngressRouteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("ingressroute", req.NamespacedName)

	// Fetch all IngressRoutes across all namespaces using an unstructured list
	// so we don't need generated CRD client code.
	irList := &unstructured.UnstructuredList{}
	irList.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   IngressRouteGVK.Group,
		Version: IngressRouteGVK.Version,
		Kind:    IngressRouteGVK.Kind + "List",
	})

	if err := r.List(ctx, irList); err != nil {
		if errors.IsNotFound(err) {
			// CRD not installed yet — requeue
			return ctrl.Result{}, nil
		}
		logger.Error(err, "failed to list IngressRoutes")
		return ctrl.Result{}, err
	}

	// Build RouteSpec slice from all active IngressRoutes
	routes, err := r.buildRouteSpecs(irList)
	if err != nil {
		logger.Error(err, "failed to build route specs")
		return ctrl.Result{}, err
	}

	// Push to xDS
	if err := r.XDS.UpdateSnapshot(r.NodeID, routes); err != nil {
		logger.Error(err, "failed to update xDS snapshot")
		return ctrl.Result{}, err
	}

	r.Logger.Info("xDS snapshot pushed",
		"nodeID", r.NodeID,
		"routes", len(routes),
		"trigger", req.NamespacedName,
	)

	// Update status on the specific IngressRoute that triggered this reconcile
	if err := r.updateStatus(ctx, req, r.XDS.SnapshotVersion(r.NodeID)); err != nil {
		// Status update failure is non-fatal
		logger.Error(err, "failed to update IngressRoute status")
	}

	return ctrl.Result{}, nil
}

// buildRouteSpecs converts an unstructured IngressRoute list into RouteSpec values.
func (r *IngressRouteReconciler) buildRouteSpecs(list *unstructured.UnstructuredList) ([]xds.RouteSpec, error) {
	routes := make([]xds.RouteSpec, 0, len(list.Items))

	for _, item := range list.Items {
		spec, found, err := unstructured.NestedMap(item.Object, "spec")
		if err != nil || !found {
			r.Logger.Warn("IngressRoute missing spec, skipping",
				"name", item.GetName(),
				"namespace", item.GetNamespace(),
			)
			continue
		}

		prefix, _, _ := unstructured.NestedString(spec, "prefix")
		svcName, _, _ := unstructured.NestedString(spec, "serviceName")
		svcPort, _, _ := unstructured.NestedFieldNoCopy(spec, "servicePort")
		authRequired, _, _ := unstructured.NestedBool(spec, "authRequired")
		timeoutMs, _, _ := unstructured.NestedFieldNoCopy(spec, "timeoutMs")
		numRetries, _, _ := unstructured.NestedFieldNoCopy(spec, "retryPolicy", "numRetries")
		perTryMs, _, _ := unstructured.NestedFieldNoCopy(spec, "retryPolicy", "perTryTimeoutMs")

		if prefix == "" || svcName == "" {
			r.Logger.Warn("IngressRoute has empty prefix or serviceName, skipping",
				"name", item.GetName(),
			)
			continue
		}

		ns := item.GetNamespace()
		if ns == "" {
			ns = "default"
		}

		routes = append(routes, xds.RouteSpec{
			Name:             item.GetName(),
			Namespace:        ns,
			Prefix:           prefix,
			ServiceName:      svcName,
			ServiceNamespace: ns,
			ClusterName:      fmt.Sprintf("%s.%s", svcName, ns),
			ServicePort:      toUint32(svcPort),
			AuthRequired:     authRequired,
			TimeoutMs:        toUint32(timeoutMs),
			NumRetries:       toUint32(numRetries),
			PerTryTimeoutMs:  toUint32(perTryMs),
		})
	}

	return routes, nil
}

// updateStatus patches the IngressRoute status with the current xDS version.
func (r *IngressRouteReconciler) updateStatus(ctx context.Context, req ctrl.Request, version string) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(IngressRouteGVK)

	if err := r.Get(ctx, req.NamespacedName, obj); err != nil {
		if errors.IsNotFound(err) {
			return nil // deleted — nothing to update
		}
		return err
	}

	// Build ready condition
	now := metav1.Now()
	condition := map[string]interface{}{
		"type":               "Ready",
		"status":             "True",
		"reason":             "XDSSnapshotUpdated",
		"message":            fmt.Sprintf("Route programmed in xDS snapshot %s", version),
		"lastTransitionTime": now.UTC().Format("2006-01-02T15:04:05Z"),
	}

	existing, found, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if !found {
		existing = []interface{}{}
	}

	// Replace or append the Ready condition
	updated := replaceOrAppendCondition(existing, condition)

	patch := obj.DeepCopy()
	if err := unstructured.SetNestedSlice(patch.Object, updated, "status", "conditions"); err != nil {
		return err
	}
	if err := unstructured.SetNestedField(patch.Object, version, "status", "xdsSnapshotVersion"); err != nil {
		return err
	}

	return r.Status().Patch(ctx, patch, client.MergeFrom(obj))
}

// replaceOrAppendCondition upserts a condition by type.
func replaceOrAppendCondition(existing []interface{}, newCond map[string]interface{}) []interface{} {
	condType, _ := newCond["type"].(string)
	for i, c := range existing {
		m, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if t, _ := m["type"].(string); t == condType {
			existing[i] = newCond
			return existing
		}
	}
	return append(existing, newCond)
}

// SetupWithManager registers this reconciler with the controller-runtime manager.
func (r *IngressRouteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// We watch unstructured IngressRoute objects since we have no generated
	// typed client — dynamic watching keeps the binary dependency-light.
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(IngressRouteGVK)

	return ctrl.NewControllerManagedBy(mgr).
		For(u).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: 1, // serialise to avoid snapshot races
		}).
		Complete(r)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func toUint32(v interface{}) uint32 {
	if v == nil {
		return 0
	}
	switch val := v.(type) {
	case int64:
		return uint32(val)
	case float64:
		return uint32(val)
	case int:
		return uint32(val)
	case int32:
		return uint32(val)
	case uint32:
		return val
	}
	return 0
}

// Ensure IngressRouteReconciler satisfies the controller-runtime Reconciler interface at compile time.
var _ interface {
	Reconcile(context.Context, ctrl.Request) (ctrl.Result, error)
} = (*IngressRouteReconciler)(nil)
