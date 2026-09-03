package controller

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/breezycourses/presage/api/v1alpha1"
	"github.com/breezycourses/presage/internal/agones"
	"github.com/breezycourses/presage/internal/forecast"
	"github.com/breezycourses/presage/internal/metrics"
	"github.com/breezycourses/presage/internal/obs"
	"github.com/breezycourses/presage/internal/policy"
	"github.com/breezycourses/presage/internal/scaletarget"
)

// finalizer lets the reconciler clean up out-of-cluster state -- the Agones
// recommendation cache and this scaler's exported metrics -- before the object
// disappears. Without it a deleted PredictiveScaler would keep answering the
// Agones webhook until its recommendation went stale.
const finalizer = "scaling.presage.sh/cleanup"

// PredictiveScalerReconciler reconciles PredictiveScaler objects.
type PredictiveScalerReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	Recorder    record.EventRecorder
	AgonesStore *agones.Store

	// HTTPClient is used to reach forecast backends.
	HTTPClient *http.Client
}

// +kubebuilder:rbac:groups=scaling.presage.sh,resources=predictivescalers,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=scaling.presage.sh,resources=predictivescalers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=scaling.presage.sh,resources=predictivescalers/finalizers,verbs=update
// +kubebuilder:rbac:groups=scaling.presage.sh,resources=forecastbackends,verbs=get;list;watch
// +kubebuilder:rbac:groups=scaling.presage.sh,resources=forecastbackends/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments/scale;statefulsets/scale;replicasets/scale,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets;replicasets,verbs=get;list;watch
// +kubebuilder:rbac:groups=agones.dev,resources=fleets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile evaluates one PredictiveScaler.
func (r *PredictiveScalerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var ps v1alpha1.PredictiveScaler
	if err := r.Get(ctx, req.NamespacedName, &ps); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !ps.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.cleanup(ctx, &ps)
	}
	if !controllerutil.ContainsFinalizer(&ps, finalizer) {
		controllerutil.AddFinalizer(&ps, finalizer)
		if err := r.Update(ctx, &ps); err != nil {
			return ctrl.Result{}, err
		}
	}

	interval := durOr(ps.Spec.Interval, defaultInterval)
	result := ctrl.Result{RequeueAfter: interval}

	if err := r.evaluate(ctx, &ps); err != nil {
		logger.Error(err, "evaluation failed")
		obs.ReconcileTotal.WithLabelValues(ps.Namespace, ps.Name, "error").Inc()
		r.event(&ps, corev1.EventTypeWarning, "EvaluationFailed", err.Error())
		setCondition(&ps, v1alpha1.ConditionReady, metav1.ConditionFalse, "EvaluationFailed", err.Error())

		// A failing scaler must stop answering the Agones webhook, so that a
		// Chain policy falls through to its fallback rather than acting on a
		// recommendation that is no longer being refreshed.
		if scaletarget.IsAgonesFleet(ps.Spec.ScaleTargetRef) && r.AgonesStore != nil {
			r.AgonesStore.Delete(ps.Namespace, ps.Spec.ScaleTargetRef.Name)
		}

		if statusErr := r.Status().Update(ctx, &ps); statusErr != nil && !apierrors.IsConflict(statusErr) {
			logger.Error(statusErr, "status update failed")
		}
		// The error is already recorded in status and metrics; returning it as
		// well would double it up with controller-runtime's own exponential
		// backoff on top of the configured interval.
		return result, nil
	}

	obs.ReconcileTotal.WithLabelValues(ps.Namespace, ps.Name, "success").Inc()
	if err := r.Status().Update(ctx, &ps); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
	}
	return result, nil
}

// evaluate runs one full cycle and mutates ps.Status. It returns an error
// without touching the target if any input is unusable: presage would rather
// leave a workload at its current size than resize it from bad data.
func (r *PredictiveScalerReconciler) evaluate(ctx context.Context, ps *v1alpha1.PredictiveScaler) error {
	now := time.Now()
	ps.Status.ObservedGeneration = ps.Generation

	target, err := r.buildTarget(ps)
	if err != nil {
		return err
	}
	current, err := target.Current(ctx)
	if err != nil {
		return err
	}
	ps.Status.CurrentReplicas = current

	labels := []string{ps.Namespace, ps.Name, target.Describe()}
	obs.CurrentReplicas.WithLabelValues(labels...).Set(float64(current))

	signals, err := resolveSignals(ps)
	if err != nil {
		return err
	}

	leadSpec := ps.Spec.LeadTime
	baseAddress := signals[0].prometheus.Address
	baseClient, err := r.metricsClient(ctx, ps.Namespace, signals[0].prometheus)
	if err != nil {
		return err
	}

	leadTime, err := r.leadTime(ctx, leadSpec, ps.Namespace, baseAddress, baseClient, now)
	if err != nil {
		return err
	}
	obs.LeadTimeSeconds.WithLabelValues(labels...).Set(leadTime.Seconds())

	targetQ, err := parseQuantile(ps.Spec.Policy.TargetQuantile, defaultTargetQuantile)
	if err != nil {
		return fmt.Errorf("spec.policy.targetQuantile: %w", err)
	}
	downQ, err := parseQuantile(ps.Spec.Policy.ScaleDownQuantile, defaultDownQuantile)
	if err != nil {
		return fmt.Errorf("spec.policy.scaleDownQuantile: %w", err)
	}

	backend, backendName, err := r.backend(ctx, ps)
	if err != nil {
		return err
	}

	policySignals := make([]policy.Signal, 0, len(signals))
	statuses := make([]v1alpha1.SignalStatus, 0, len(signals))
	samples := make(map[string]*v1alpha1.ForecastSample, len(signals))

	for _, sig := range signals {
		client := baseClient
		if sig.prometheus.Address != baseAddress {
			if client, err = r.metricsClient(ctx, ps.Namespace, sig.prometheus); err != nil {
				return fmt.Errorf("signal %q: %w", sig.name, err)
			}
		}

		// Align the range to the resolution grid so successive reconciles ask
		// for the same buckets. Without this every reconcile shifts the grid by
		// a few seconds and the model sees a subtly different series each time.
		end := now.Truncate(sig.resolution)
		start := end.Add(-sig.history)

		series, err := client.QueryRange(ctx, sig.prometheus.Query, start, end, sig.resolution)
		if err != nil {
			return fmt.Errorf("signal %q: %w", sig.name, err)
		}
		obs.SignalGaps.WithLabelValues(ps.Namespace, ps.Name, sig.name).Set(float64(series.Gaps))

		// A series that is mostly interpolation is not evidence of anything.
		if gapRatio := float64(series.Gaps) / float64(series.Steps); gapRatio > maxGapRatio {
			return fmt.Errorf("signal %q is %.0f%% gap-filled over %s; "+
				"check the query, the scrape interval, or lower the signal resolution",
				sig.name, gapRatio*100, sig.history)
		}

		currentDemand := series.Values[len(series.Values)-1]
		obs.SignalValue.WithLabelValues(ps.Namespace, ps.Name, sig.name).Set(currentDemand)

		perReplica, err := r.perReplicaCapacity(ctx, sig.capacity, ps.Namespace, baseAddress, client, now)
		if err != nil {
			return fmt.Errorf("signal %q: %w", sig.name, err)
		}

		leadSteps := forecast.StepsFor(leadTime, sig.resolution)
		horizon := durOr(horizonOf(ps), time.Duration(defaultHorizonFactor)*leadTime)
		horizonSteps := forecast.StepsFor(horizon, sig.resolution)
		if horizonSteps < leadSteps {
			horizonSteps = leadSteps
		}

		started := time.Now()
		result, err := backend.Forecast(ctx, forecast.Request{
			Series: forecast.Series{
				ID:         fmt.Sprintf("%s/%s/%s", ps.Namespace, ps.Name, sig.name),
				Values:     series.Values,
				Resolution: sig.resolution,
				End:        end,
			},
			Horizon:   horizonSteps,
			Quantiles: []float64{downQ, targetQ},
		})
		obs.ForecastDuration.WithLabelValues(backendName).Observe(time.Since(started).Seconds())
		if err != nil {
			obs.ForecastErrorTotal.WithLabelValues(backendName).Inc()
			setCondition(ps, v1alpha1.ConditionForecasting, metav1.ConditionFalse, "ForecastFailed", err.Error())
			return fmt.Errorf("signal %q: %w", sig.name, err)
		}

		upValue, err := result.QuantileAt(targetQ, leadSteps)
		if err != nil {
			return fmt.Errorf("signal %q: %w", sig.name, err)
		}
		downValue, err := result.QuantileAt(downQ, leadSteps)
		if err != nil {
			return fmt.Errorf("signal %q: %w", sig.name, err)
		}
		point, _, _ := result.At(leadSteps)

		obs.ForecastValue.WithLabelValues(ps.Namespace, ps.Name, sig.name, fmt.Sprintf("%g", targetQ)).Set(upValue)
		obs.ForecastValue.WithLabelValues(ps.Namespace, ps.Name, sig.name, fmt.Sprintf("%g", downQ)).Set(downValue)

		policySignals = append(policySignals, policy.Signal{
			Name:          sig.name,
			PerReplica:    perReplica,
			ForecastUp:    upValue,
			ForecastDown:  downValue,
			CurrentDemand: currentDemand,
		})
		statuses = append(statuses, v1alpha1.SignalStatus{
			Name:     sig.name,
			Replicas: int32(math.Ceil(upValue / perReplica)), //nolint:gosec // replica counts are small
			Observed: fmt.Sprintf("%.3f", currentDemand),
			Forecast: fmt.Sprintf("%.3f", upValue),
			GapSteps: int32(series.Gaps), //nolint:gosec // bounded by the window
		})

		samples[sig.name] = &v1alpha1.ForecastSample{
			GeneratedAt:     metav1.NewTime(now),
			Backend:         result.Backend,
			Model:           result.Model,
			Revision:        result.Revision,
			LeadTimeSeconds: int32(leadTime.Seconds()), //nolint:gosec // clamped above
			Point:           fmt.Sprintf("%.3f", point),
			Quantiles: map[string]string{
				fmt.Sprintf("%g", targetQ): fmt.Sprintf("%.3f", upValue),
				fmt.Sprintf("%g", downQ):   fmt.Sprintf("%.3f", downValue),
			},
		}
	}

	setCondition(ps, v1alpha1.ConditionForecasting, metav1.ConditionTrue, "ForecastReady",
		fmt.Sprintf("%s forecast %d signal(s)", backendName, len(signals)))

	cfg, err := policyConfig(ps)
	if err != nil {
		return err
	}

	var candidateSince *time.Time
	if ps.Status.ScaleDownCandidateSince != nil {
		t := ps.Status.ScaleDownCandidateSince.Time
		candidateSince = &t
	}

	decision, err := policy.Evaluate(cfg, policy.Input{
		Now:                     now,
		CurrentReplicas:         current,
		Signals:                 policySignals,
		ScaleDownCandidateSince: candidateSince,
	})
	if err != nil {
		return err
	}

	for _, sig := range decision.CrossingSignals {
		obs.CrossingQuantilesTotal.WithLabelValues(ps.Namespace, ps.Name, sig).Inc()
	}

	ps.Status.SignalStatuses = statuses
	ps.Status.RecommendedReplicas = decision.Replicas
	ps.Status.Breakdown = &v1alpha1.RecommendationBreakdown{
		Predictive:    decision.Predictive,
		Reactive:      decision.Reactive,
		Constraint:    string(decision.Constraint),
		BindingSignal: decision.BindingSignal,
	}
	// Report the forecast that actually set the target, not an arbitrary one:
	// with several signals, the interesting forecast is the binding one.
	if sample, ok := samples[decision.BindingSignal]; ok {
		ps.Status.LastForecast = sample
	}
	if decision.ScaleDownCandidateSince != nil {
		ps.Status.ScaleDownCandidateSince = &metav1.Time{Time: *decision.ScaleDownCandidateSince}
	} else {
		ps.Status.ScaleDownCandidateSince = nil
	}

	obs.RecommendedReplicas.WithLabelValues(labels...).Set(float64(decision.Replicas))
	obs.PredictiveReplicas.WithLabelValues(labels...).Set(float64(decision.Predictive))
	if decision.Reactive != nil {
		obs.ReactiveReplicas.WithLabelValues(labels...).Set(float64(*decision.Reactive))
	}
	obs.ConstraintTotal.WithLabelValues(ps.Namespace, ps.Name,
		constraintLabel(decision.Constraint)).Inc()

	if ps.Spec.Mode != v1alpha1.ModeEnforce {
		// Shadow mode still publishes to the Agones store, flagged advisory,
		// so the webhook can report *why* it is not scaling rather than
		// looking like an outage.
		if af, ok := target.(*scaletarget.AgonesFleet); ok {
			af.Shadow = true
			_ = af.Apply(ctx, decision.Replicas)
		}
		setCondition(ps, v1alpha1.ConditionScalingActive, metav1.ConditionFalse, "ShadowMode",
			"recommendation published but not applied")
		setCondition(ps, v1alpha1.ConditionReady, metav1.ConditionTrue, "Evaluated", decision.Explain)
		return nil
	}

	if err := target.Apply(ctx, decision.Replicas); err != nil {
		setCondition(ps, v1alpha1.ConditionScalingActive, metav1.ConditionFalse, "ApplyFailed", err.Error())
		return err
	}
	if decision.Replicas != current {
		direction := "up"
		if decision.Replicas < current {
			direction = "down"
		}
		obs.ScaleTotal.WithLabelValues(ps.Namespace, ps.Name, direction).Inc()
		ps.Status.LastScaleTime = &metav1.Time{Time: now}
		r.event(ps, corev1.EventTypeNormal, "Scaled",
			fmt.Sprintf("%s %d -> %d: %s", target.Describe(), current, decision.Replicas, decision.Explain))
	}

	setCondition(ps, v1alpha1.ConditionScalingActive, metav1.ConditionTrue, "Enforcing",
		fmt.Sprintf("applying recommendations to %s", target.Describe()))
	setCondition(ps, v1alpha1.ConditionReady, metav1.ConditionTrue, "Evaluated", decision.Explain)
	return nil
}

func (r *PredictiveScalerReconciler) cleanup(ctx context.Context, ps *v1alpha1.PredictiveScaler) error {
	if scaletarget.IsAgonesFleet(ps.Spec.ScaleTargetRef) && r.AgonesStore != nil {
		r.AgonesStore.Delete(ps.Namespace, ps.Spec.ScaleTargetRef.Name)
	}
	obs.Forget(ps.Namespace, ps.Name,
		fmt.Sprintf("%s %s/%s", ps.Spec.ScaleTargetRef.Kind, ps.Namespace, ps.Spec.ScaleTargetRef.Name))

	if controllerutil.RemoveFinalizer(ps, finalizer) {
		return r.Update(ctx, ps)
	}
	return nil
}

// resolvedSignal is the normalised form of both spec shapes, so the rest of
// the reconciler never has to care which one the user wrote.
type resolvedSignal struct {
	name       string
	prometheus *v1alpha1.PrometheusSignal
	resolution time.Duration
	history    time.Duration
	capacity   *v1alpha1.CapacitySpec
}

// resolveSignals flattens spec.signal or spec.signals into a single list.
func resolveSignals(ps *v1alpha1.PredictiveScaler) ([]resolvedSignal, error) {
	hasSingle := ps.Spec.Signal != nil
	hasMulti := len(ps.Spec.Signals) > 0

	switch {
	case hasSingle && hasMulti:
		return nil, fmt.Errorf("set exactly one of spec.signal or spec.signals, not both")
	case !hasSingle && !hasMulti:
		return nil, fmt.Errorf("one of spec.signal or spec.signals is required")
	}

	if hasSingle {
		sig := ps.Spec.Signal
		if sig.Prometheus == nil {
			return nil, fmt.Errorf("spec.signal.prometheus is required")
		}
		return []resolvedSignal{{
			name:       "default",
			prometheus: sig.Prometheus,
			resolution: durOr(sig.Resolution, defaultResolution),
			history:    durOr(sig.History, defaultHistory),
			capacity:   ps.Spec.Capacity,
		}}, nil
	}

	out := make([]resolvedSignal, 0, len(ps.Spec.Signals))
	seen := map[string]bool{}
	for i := range ps.Spec.Signals {
		sig := &ps.Spec.Signals[i]
		if seen[sig.Name] {
			return nil, fmt.Errorf("duplicate signal name %q", sig.Name)
		}
		seen[sig.Name] = true
		if sig.Prometheus == nil {
			return nil, fmt.Errorf("spec.signals[%q].prometheus is required", sig.Name)
		}
		capacity := sig.Capacity
		out = append(out, resolvedSignal{
			name:       sig.Name,
			prometheus: sig.Prometheus,
			resolution: durOr(sig.Resolution, defaultResolution),
			history:    durOr(sig.History, defaultHistory),
			capacity:   &capacity,
		})
	}
	return out, nil
}

func (r *PredictiveScalerReconciler) buildTarget(ps *v1alpha1.PredictiveScaler) (scaletarget.Target, error) {
	key := types.NamespacedName{Namespace: ps.Namespace, Name: ps.Spec.ScaleTargetRef.Name}

	if scaletarget.IsAgonesFleet(ps.Spec.ScaleTargetRef) {
		if r.AgonesStore == nil {
			return nil, fmt.Errorf("target is an Agones Fleet but the webhook server is disabled")
		}
		maxAge := defaultAgonesMaxAge
		if a := ps.Spec.ScaleTargetRef.Agones; a != nil {
			maxAge = durOr(a.MaxRecommendationAge, defaultAgonesMaxAge)
		}
		return &scaletarget.AgonesFleet{
			Client: r.Client,
			Store:  r.AgonesStore,
			Key:    key,
			MaxAge: maxAge,
			Shadow: ps.Spec.Mode != v1alpha1.ModeEnforce,
		}, nil
	}

	return &scaletarget.ScaleSubresource{
		Client: r.Client,
		Ref:    ps.Spec.ScaleTargetRef,
		Key:    key,
	}, nil
}

// leadTime resolves the provisioning lead time, always clamped. An Observed
// query that returns nonsense must not be able to collapse the horizon to
// zero, which would silently turn presage into a reactive autoscaler.
func (r *PredictiveScalerReconciler) leadTime(
	ctx context.Context,
	spec *v1alpha1.LeadTimeSpec,
	namespace string,
	baseAddress string,
	fallbackClient *metrics.Client,
	now time.Time,
) (time.Duration, error) {
	if spec == nil {
		return defaultLeadTime, nil
	}
	lo := durOr(spec.Min, defaultLeadTimeMin)
	hi := durOr(spec.Max, defaultLeadTimeMax)
	if lo > hi {
		return 0, fmt.Errorf("spec.leadTime.min (%s) exceeds max (%s)", lo, hi)
	}

	if spec.Source != v1alpha1.LeadTimeObserved {
		return clampDuration(durOr(spec.Static, defaultLeadTime), lo, hi), nil
	}
	if spec.Observed == nil {
		return 0, fmt.Errorf("spec.leadTime.source is Observed but spec.leadTime.observed is unset")
	}

	c := fallbackClient
	if spec.Observed.Address != "" && spec.Observed.Address != baseAddress {
		var err error
		if c, err = r.metricsClient(ctx, namespace, spec.Observed); err != nil {
			return 0, err
		}
	}
	seconds, err := c.QueryScalar(ctx, spec.Observed.Query, now)
	if err != nil {
		return 0, fmt.Errorf("observed lead time: %w", err)
	}
	return clampDuration(time.Duration(seconds*float64(time.Second)), lo, hi), nil
}

func (r *PredictiveScalerReconciler) perReplicaCapacity(
	ctx context.Context,
	spec *v1alpha1.CapacitySpec,
	namespace string,
	baseAddress string,
	fallbackClient *metrics.Client,
	now time.Time,
) (float64, error) {
	if spec == nil {
		return 1, nil
	}
	if spec.Query != nil {
		c := fallbackClient
		if spec.Query.Address != "" && spec.Query.Address != baseAddress {
			var err error
			if c, err = r.metricsClient(ctx, namespace, spec.Query); err != nil {
				return 0, err
			}
		}
		v, err := c.QueryScalar(ctx, spec.Query.Query, now)
		if err != nil {
			return 0, fmt.Errorf("capacity query: %w", err)
		}
		if v <= 0 {
			return 0, fmt.Errorf("capacity query returned %v; per-replica capacity must be > 0", v)
		}
		return v, nil
	}
	if spec.PerReplica != nil {
		v := spec.PerReplica.AsApproximateFloat64()
		if v <= 0 {
			return 0, fmt.Errorf("spec.capacity.perReplica must be > 0, got %v", v)
		}
		return v, nil
	}
	return 1, nil
}

func (r *PredictiveScalerReconciler) metricsClient(
	ctx context.Context, namespace string, spec *v1alpha1.PrometheusSignal,
) (*metrics.Client, error) {
	var token string
	if ref := spec.BearerTokenSecretRef; ref != nil {
		var secret corev1.Secret
		if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: ref.Name}, &secret); err != nil {
			return nil, fmt.Errorf("read secret %s/%s: %w", namespace, ref.Name, err)
		}
		raw, ok := secret.Data[ref.Key]
		if !ok {
			return nil, fmt.Errorf("secret %s/%s has no key %q", namespace, ref.Name, ref.Key)
		}
		token = string(raw)
	}
	return metrics.NewClient(spec.Address, token, spec.InsecureSkipVerify, 30*time.Second), nil
}

func (r *PredictiveScalerReconciler) backend(
	ctx context.Context, ps *v1alpha1.PredictiveScaler,
) (forecast.Backend, string, error) {
	name := "default"
	if ps.Spec.Forecast != nil && ps.Spec.Forecast.BackendRef != nil {
		name = *ps.Spec.Forecast.BackendRef
	}

	var fb v1alpha1.ForecastBackend
	if err := r.Get(ctx, types.NamespacedName{Name: name}, &fb); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, "", fmt.Errorf("ForecastBackend %q not found", name)
		}
		return nil, "", err
	}

	switch fb.Spec.Type {
	case v1alpha1.BackendTimesFM:
		if fb.Spec.TimesFM == nil {
			return nil, "", fmt.Errorf("ForecastBackend %q is type TimesFM but spec.timesFM is unset", name)
		}
		t := fb.Spec.TimesFM
		httpClient := r.HTTPClient
		if httpClient == nil {
			httpClient = http.DefaultClient
		}
		// Per-backend timeout, layered over the shared client.
		client := *httpClient
		client.Timeout = durOr(t.Timeout, 30*time.Second)
		return &forecast.TimesFM{
			Endpoint:   t.Endpoint,
			MaxContext: int(t.MaxContext),
			MaxHorizon: int(t.MaxHorizon),
			Client:     &client,
		}, string(fb.Spec.Type), nil

	case v1alpha1.BackendSeasonalNaive:
		sn := &forecast.SeasonalNaive{Season: 168 * time.Hour, Cycles: 3}
		if s := fb.Spec.SeasonalNaive; s != nil {
			sn.Season = durOr(s.Season, sn.Season)
			if s.Cycles > 0 {
				sn.Cycles = int(s.Cycles)
			}
		}
		return sn, string(fb.Spec.Type), nil

	default:
		return nil, "", fmt.Errorf("ForecastBackend %q has unknown type %q", name, fb.Spec.Type)
	}
}

func policyConfig(ps *v1alpha1.PredictiveScaler) (policy.Config, error) {
	p := ps.Spec.Policy

	headroom, err := parseAmount(p.Headroom, policy.Amount{Value: 10, Percent: true})
	if err != nil {
		return policy.Config{}, fmt.Errorf("spec.policy.headroom: %w", err)
	}

	cfg := policy.Config{
		Headroom:         headroom,
		MinReplicas:      p.MinReplicas,
		MaxReplicas:      p.MaxReplicas,
		ScaleDownWindow:  defaultScaleDownWindow,
		MaxScaleUpRate:   policy.Amount{Value: 100, Percent: true},
		MaxScaleDownRate: policy.Amount{Value: 20, Percent: true},
	}

	if s := p.Stabilization; s != nil {
		cfg.ScaleDownWindow = durOr(s.ScaleDownWindow, defaultScaleDownWindow)
		if cfg.MaxScaleUpRate, err = parseAmount(s.MaxScaleUpRate, cfg.MaxScaleUpRate); err != nil {
			return policy.Config{}, fmt.Errorf("spec.policy.stabilization.maxScaleUpRate: %w", err)
		}
		if cfg.MaxScaleDownRate, err = parseAmount(s.MaxScaleDownRate, cfg.MaxScaleDownRate); err != nil {
			return policy.Config{}, fmt.Errorf("spec.policy.stabilization.maxScaleDownRate: %w", err)
		}
	}

	if f := p.ReactiveFloor; f == nil || boolOr(f.Enabled, true) {
		buffer := policy.Amount{Value: 10, Percent: true}
		if f != nil {
			if buffer, err = parseAmount(f.BufferSize, buffer); err != nil {
				return policy.Config{}, fmt.Errorf("spec.policy.reactiveFloor.bufferSize: %w", err)
			}
		}
		cfg.ReactiveFloor = &policy.ReactiveFloor{Buffer: buffer}
	}

	cfg.ScaleDownMaxRelativeSpread = defaultMaxSpread
	if g := p.ScaleDownUncertaintyGuard; g != nil {
		if !boolOr(g.Enabled, true) {
			cfg.ScaleDownMaxRelativeSpread = 0
		} else if cfg.ScaleDownMaxRelativeSpread, err = parseRatio(g.MaxRelativeSpread, defaultMaxSpread); err != nil {
			return policy.Config{}, fmt.Errorf("spec.policy.scaleDownUncertaintyGuard.maxRelativeSpread: %w", err)
		}
	}

	return cfg, nil
}

func horizonOf(ps *v1alpha1.PredictiveScaler) *metav1.Duration {
	if ps.Spec.Forecast == nil {
		return nil
	}
	return ps.Spec.Forecast.Horizon
}

func constraintLabel(c policy.Constraint) string {
	if c == policy.ConstraintNone {
		return "Forecast"
	}
	return string(c)
}

func setCondition(ps *v1alpha1.PredictiveScaler, condType string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&ps.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            truncateMessage(message),
		ObservedGeneration: ps.Generation,
	})
}

// truncateMessage keeps condition messages inside the API server's 32KiB
// limit; the policy explanation can get long.
func truncateMessage(s string) string {
	const max = 1024
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func (r *PredictiveScalerReconciler) event(ps *v1alpha1.PredictiveScaler, eventType, reason, message string) {
	if r.Recorder != nil {
		r.Recorder.Event(ps, eventType, reason, truncateMessage(message))
	}
}

// SetupWithManager wires the reconciler into the manager.
func (r *PredictiveScalerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.PredictiveScaler{}).
		Named("predictivescaler").
		Complete(r)
}
