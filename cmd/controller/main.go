// main.go — Ingress Control Plane Engine entry point.
//
// Startup sequence:
//  1. Parse configuration from environment variables / flags
//  2. Build a controller-runtime Manager (leader election, health probes)
//  3. Start the Envoy xDS gRPC server in a goroutine
//  4. Register the IngressRoute reconciler with the Manager
//  5. Block on Manager.Start() — handles OS signals for graceful shutdown
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/mphasis/ingress-control-plane-engine/pkg/controller"
	"github.com/mphasis/ingress-control-plane-engine/pkg/xds"
)

// ---------------------------------------------------------------------------
// Scheme — register core Kubernetes types
// ---------------------------------------------------------------------------

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
}

// ---------------------------------------------------------------------------
// Config — populated from flags / env
// ---------------------------------------------------------------------------

type config struct {
	xdsAddr     string
	metricsAddr string
	probeAddr   string
	leaderElect bool
	nodeID      string
	logLevel    string
	syncPeriod  time.Duration
}

func parseConfig() config {
	cfg := config{}
	flag.StringVar(&cfg.xdsAddr, "xds-addr", envOr("XDS_ADDR", ":18000"),
		"Address the xDS gRPC server listens on (e.g. :18000)")
	flag.StringVar(&cfg.metricsAddr, "metrics-addr", envOr("METRICS_ADDR", ":8080"),
		"Address to expose Prometheus metrics on")
	flag.StringVar(&cfg.probeAddr, "probe-addr", envOr("PROBE_ADDR", ":8081"),
		"Address to expose /healthz and /readyz probes on")
	flag.BoolVar(&cfg.leaderElect, "leader-elect", envBool("LEADER_ELECT", true),
		"Enable leader election for multi-replica HA deployments")
	flag.StringVar(&cfg.nodeID, "node-id", envOr("ENVOY_NODE_ID", xds.DefaultNodeID),
		"Envoy node ID this control plane manages")
	flag.StringVar(&cfg.logLevel, "log-level", envOr("LOG_LEVEL", "info"),
		"Log level: debug | info | warn | error")
	flag.DurationVar(&cfg.syncPeriod, "sync-period", 10*time.Minute,
		"Full re-sync period for the controller cache")
	flag.Parse()
	return cfg
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() {
	cfg := parseConfig()

	// Structured JSON logger — used throughout the application
	logger := buildLogger(cfg.logLevel)
	slog.SetDefault(logger)

	// controller-runtime uses its own zap logger; align it with our level
	ctrl.SetLogger(zap.New(zap.UseDevMode(cfg.logLevel == "debug")))

	logger.Info("Starting Ingress Control Plane Engine",
		"xdsAddr", cfg.xdsAddr,
		"metricsAddr", cfg.metricsAddr,
		"nodeID", cfg.nodeID,
		"leaderElect", cfg.leaderElect,
	)

	// -----------------------------------------------------------------------
	// 1. xDS Server
	// -----------------------------------------------------------------------
	xdsSrv := xds.NewServer(logger)

	// Push an empty initial snapshot so Envoy doesn't hang waiting for one
	if err := xdsSrv.UpdateSnapshot(cfg.nodeID, nil); err != nil {
		logger.Error("failed to push initial xDS snapshot", "error", err)
		os.Exit(1)
	}

	// -----------------------------------------------------------------------
	// 2. controller-runtime Manager
	// -----------------------------------------------------------------------
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: cfg.metricsAddr,
		},
		HealthProbeBindAddress: cfg.probeAddr,
		LeaderElection:         cfg.leaderElect,
		LeaderElectionID:       "ingress-control-plane-engine.platform.internal",
		// Resync all objects every syncPeriod even without Kubernetes events
		// This is the safety net against any missed watch events.
	})
	if err != nil {
		logger.Error("unable to create controller manager", "error", err)
		os.Exit(1)
	}

	// Health / readiness probes
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		logger.Error("unable to set up healthz check", "error", err)
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		logger.Error("unable to set up readyz check", "error", err)
		os.Exit(1)
	}

	// -----------------------------------------------------------------------
	// 3. Register IngressRoute reconciler
	// -----------------------------------------------------------------------
	reconciler := &controller.IngressRouteReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		XDS:    xdsSrv,
		NodeID: cfg.nodeID,
		Logger: logger,
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		logger.Error("unable to register IngressRoute reconciler", "error", err)
		os.Exit(1)
	}

	// -----------------------------------------------------------------------
	// 4. Start xDS gRPC server (non-blocking — runs in background goroutine)
	// -----------------------------------------------------------------------
	ctx := ctrl.SetupSignalHandler()
	go func() {
		if err := xdsSrv.Start(ctx, cfg.xdsAddr); err != nil {
			logger.Error("xDS server exited with error", "error", err)
			os.Exit(1)
		}
	}()

	// -----------------------------------------------------------------------
	// 5. Start controller manager (blocks until SIGTERM / SIGINT)
	// -----------------------------------------------------------------------
	logger.Info("Control plane ready — waiting for IngressRoute events")
	if err := mgr.Start(ctx); err != nil {
		logger.Error("controller manager exited with error", "error", err)
		os.Exit(1)
	}

	logger.Info("Control plane shut down cleanly")
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func buildLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "false" || v == "0" {
		return false
	}
	if v == "true" || v == "1" {
		return true
	}
	return fallback
}

// Ensure fmt is used (package referenced by xds internally).
var _ = fmt.Sprintf
