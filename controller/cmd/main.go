// vpa-gate: lets VPA resize running pods only after the HPA has settled.
package main

import (
	"flag"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/nitin1393/pubsub-autoscaling/controller/internal/controller"
	"github.com/nitin1393/pubsub-autoscaling/controller/internal/gate"
)

func main() {
	var (
		metricsAddr, probeAddr, watchNS string
		leaderNS                        string
		scalingMode, settledMode        string
		stable, resync                  time.Duration
		leaderElect, dryRun             bool
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Prometheus metrics address.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Health probe address.")
	flag.StringVar(&watchNS, "watch-namespace", os.Getenv("WATCH_NAMESPACE"),
		"Namespace to watch (empty = all namespaces; needs a ClusterRole).")
	flag.DurationVar(&stable, "stable-window", 15*time.Minute,
		"Default time the HPA must be unchanged before VPA may resize running pods.")
	flag.DurationVar(&resync, "resync-period", 10*time.Minute, "Full resync safety net.")
	flag.StringVar(&scalingMode, "scaling-mode", gate.DefaultModes.Scaling, "VPA updateMode while the HPA is scaling.")
	flag.StringVar(&settledMode, "settled-mode", gate.DefaultModes.Settled, "VPA updateMode once the HPA has settled.")
	flag.BoolVar(&leaderElect, "leader-elect", true, "Enable leader election (safe to run 2 replicas).")
	flag.StringVar(&leaderNS, "leader-election-namespace", os.Getenv("POD_NAMESPACE"),
		"Namespace for the leader-election lease (default: POD_NAMESPACE, else the watched namespace).")
	flag.BoolVar(&dryRun, "dry-run", false, "Log decisions without patching VPAs.")
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	cacheOpts := cache.Options{SyncPeriod: &resync}
	if watchNS != "" {
		cacheOpts.DefaultNamespaces = map[string]cache.Config{watchNS: {}}
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                  scheme,
		Cache:                   cacheOpts,
		Metrics:                 metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress:  probeAddr,
		LeaderElection:          leaderElect,
		LeaderElectionID:        "vpa-gate.pubsub-autoscaling",
		LeaderElectionNamespace: leaderNamespace(leaderNS, watchNS),
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	r := &controller.Reconciler{
		Client:        mgr.GetClient(),
		Recorder:      mgr.GetEventRecorderFor("vpa-gate"),
		DefaultStable: stable,
		Modes:         gate.Modes{Scaling: scalingMode, Settled: settledMode},
		DryRun:        dryRun,
	}
	if err := r.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to set up controller")
		os.Exit(1)
	}
	utilruntime.Must(mgr.AddHealthzCheck("healthz", healthz.Ping))
	utilruntime.Must(mgr.AddReadyzCheck("readyz", healthz.Ping))

	setupLog.Info("starting vpa-gate", "namespace", watchNS, "stableWindow", stable,
		"scalingMode", scalingMode, "settledMode", settledMode, "dryRun", dryRun)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "manager exited")
		os.Exit(1)
	}
}

// leaderNamespace picks where the leader-election lease lives: the explicit
// flag/POD_NAMESPACE if set, else the watched namespace. Empty lets
// controller-runtime use the pod's own namespace.
func leaderNamespace(explicit, watched string) string {
	if explicit != "" {
		return explicit
	}
	return watched
}
