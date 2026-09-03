// Command manager runs the presage controller and, optionally, the Agones
// FleetAutoscaler webhook server.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"net/http"
	"os"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/breezycourses/presage/api/v1alpha1"
	"github.com/breezycourses/presage/internal/agones"
	"github.com/breezycourses/presage/internal/controller"
	"github.com/breezycourses/presage/internal/obs"
)

var scheme = runtime.NewScheme()

func init() {
	utilRuntimeMust(clientgoscheme.AddToScheme(scheme))
	utilRuntimeMust(v1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr          string
		probeAddr            string
		enableLeaderElection bool
		agonesWebhookAddr    string
		agonesWebhookEnabled bool
		agonesDefaultMaxAge  time.Duration
		tlsCertFile          string
		tlsKeyFile           string
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080",
		"Address the metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081",
		"Address the health probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election, so only one replica reconciles at a time.")
	flag.BoolVar(&agonesWebhookEnabled, "agones-webhook", true,
		"Serve the Agones FleetAutoscaler webhook.")
	flag.StringVar(&agonesWebhookAddr, "agones-webhook-bind-address", ":8000",
		"Address the Agones FleetAutoscaler webhook binds to. Agones defaults to port 8000 for service-based webhooks.")
	flag.DurationVar(&agonesDefaultMaxAge, "agones-max-recommendation-age", 5*time.Minute,
		"Default staleness limit for cached recommendations; past this the webhook errors so a Chain policy falls through.")
	flag.StringVar(&tlsCertFile, "agones-webhook-tls-cert", "",
		"Optional TLS certificate for the Agones webhook. Requires -agones-webhook-tls-key.")
	flag.StringVar(&tlsKeyFile, "agones-webhook-tls-key", "",
		"Optional TLS key for the Agones webhook.")

	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")

	if (tlsCertFile == "") != (tlsKeyFile == "") {
		setupLog.Error(errors.New("incomplete TLS configuration"),
			"both -agones-webhook-tls-cert and -agones-webhook-tls-key must be set together")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "presage.scaling.presage.sh",
		// Keep the lease alive across a slow forecast: a leader that loses its
		// lease mid-cycle would hand over to a replica with no cached
		// recommendations, and every Agones webhook call would fall through
		// until the new leader had reconciled everything.
		LeaseDuration: durationPtr(60 * time.Second),
		RenewDeadline: durationPtr(45 * time.Second),
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	var store *agones.Store
	if agonesWebhookEnabled {
		store = agones.NewStore(agonesDefaultMaxAge)
		handler := &agones.Handler{
			Store: store,
			OnServed: func(namespace, name string, served bool, reason string) {
				obs.WebhookTotal.WithLabelValues(namespace, name,
					boolLabel(served), reason).Inc()
			},
		}
		if err := mgr.Add(&webhookServer{
			addr:     agonesWebhookAddr,
			handler:  handler,
			certFile: tlsCertFile,
			keyFile:  tlsKeyFile,
			log:      ctrl.Log.WithName("agones-webhook"),
		}); err != nil {
			setupLog.Error(err, "unable to add the Agones webhook server")
			os.Exit(1)
		}
	}

	if err := (&controller.PredictiveScalerReconciler{
		Client:      mgr.GetClient(),
		Scheme:      mgr.GetScheme(),
		Recorder:    mgr.GetEventRecorderFor("presage"),
		AgonesStore: store,
		HTTPClient:  &http.Client{Timeout: 30 * time.Second},
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create the PredictiveScaler controller")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up the health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up the ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager", "agonesWebhook", agonesWebhookEnabled)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "manager exited with an error")
		os.Exit(1)
	}
}

// webhookServer runs the Agones FleetAutoscaler webhook as a manager Runnable
// so it shares the manager's lifecycle and shuts down cleanly.
type webhookServer struct {
	addr     string
	handler  http.Handler
	certFile string
	keyFile  string
	log      logr.Logger
}

// NeedLeaderElection reports false: every replica serves the webhook.
//
// This looks wrong at first glance and is deliberate. Only the leader
// reconciles and therefore only the leader has fresh recommendations, but the
// webhook Service load-balances across all replicas. A non-leader answers 503,
// which is precisely the signal that makes an Agones Chain policy fall through
// to its fallback -- a safe answer, and a much better one than a connection
// refused.
func (s *webhookServer) NeedLeaderElection() bool { return false }

func (s *webhookServer) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.Handle("/scale", s.handler)
	mux.Handle("/scale/", s.handler)

	srv := &http.Server{
		Addr:              s.addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// Agones' sync period is 30s by default; nothing here should ever take
		// close to that, since the handler only reads a cached value.
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
	}
	if s.certFile != "" {
		srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}

	errCh := make(chan error, 1)
	go func() {
		s.log.Info("serving Agones FleetAutoscaler webhook", "addr", s.addr, "tls", s.certFile != "")
		var err error
		if s.certFile != "" {
			err = srv.ListenAndServeTLS(s.certFile, s.keyFile)
		} else {
			err = srv.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

var _ manager.Runnable = (*webhookServer)(nil)
var _ manager.LeaderElectionRunnable = (*webhookServer)(nil)

func durationPtr(d time.Duration) *time.Duration { return &d }

func boolLabel(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func utilRuntimeMust(err error) {
	if err != nil {
		panic(err)
	}
}
