// Package obs holds the Prometheus metrics presage exports about itself.
//
// These are not decoration. Shadow mode is the intended way to adopt presage,
// and shadow mode is only useful if the recommendation is recorded next to
// what actually happened -- so `presage_recommended_replicas` beside
// `presage_current_replicas` is the whole evaluation apparatus.
package obs

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var scalerLabels = []string{"namespace", "name", "target"}

var (
	// RecommendedReplicas is what presage would apply. In Shadow mode this is
	// published and nothing else happens, which is exactly the point: compare
	// it against CurrentReplicas over a few weeks before enforcing.
	RecommendedReplicas = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "presage_recommended_replicas",
		Help: "Replica count presage recommends (published in Shadow mode, applied in Enforce mode).",
	}, scalerLabels)

	// CurrentReplicas is what the target actually has.
	CurrentReplicas = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "presage_current_replicas",
		Help: "Replica count currently observed on the target.",
	}, scalerLabels)

	// PredictiveReplicas is the forecast's unconstrained opinion, before the
	// floor, stabilization, rate limits, and clamps. The gap between this and
	// RecommendedReplicas shows how much work the safety machinery is doing.
	PredictiveReplicas = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "presage_predictive_replicas",
		Help: "Replica count implied by the forecast alone, before any constraint.",
	}, scalerLabels)

	// ReactiveReplicas is what a conventional reactive buffer policy would
	// have chosen from present demand -- the baseline presage must beat.
	ReactiveReplicas = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "presage_reactive_replicas",
		Help: "Replica count the reactive floor would have chosen from present demand.",
	}, scalerLabels)

	// ForecastValue is the forecast demand at the lead time, per quantile.
	ForecastValue = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "presage_forecast_value",
		Help: "Forecast signal value at the lead time, by quantile.",
	}, []string{"namespace", "name", "signal", "quantile"})

	// SignalValue is the most recent observed value of the signal, so the
	// forecast can be scored against it without a second data source.
	SignalValue = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "presage_signal_value",
		Help: "Most recent observed value of the signal driving the forecast.",
	}, []string{"namespace", "name", "signal"})

	// LeadTimeSeconds is the horizon actually used.
	LeadTimeSeconds = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "presage_lead_time_seconds",
		Help: "Provisioning lead time used as the forecast horizon.",
	}, scalerLabels)

	// ConstraintTotal counts which rule bound the recommendation. A scaler
	// permanently pinned at MaxReplicas or ScaleDownWindow is misconfigured,
	// and this is how that becomes visible.
	ConstraintTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "presage_constraint_total",
		Help: "Count of evaluations by the constraint that bound the recommendation.",
	}, []string{"namespace", "name", "constraint"})

	// ReconcileTotal counts reconciles by outcome.
	ReconcileTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "presage_reconcile_total",
		Help: "Count of reconciles by outcome.",
	}, []string{"namespace", "name", "result"})

	// ScaleTotal counts applied scale operations by direction.
	ScaleTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "presage_scale_total",
		Help: "Count of scale operations actually applied, by direction.",
	}, []string{"namespace", "name", "direction"})

	// ForecastDuration measures backend latency.
	ForecastDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "presage_forecast_duration_seconds",
		Help:    "Time spent producing a forecast.",
		Buckets: []float64{.01, .05, .1, .25, .5, 1, 2.5, 5, 10, 30},
	}, []string{"backend"})

	// ForecastErrorTotal counts forecast failures by backend.
	ForecastErrorTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "presage_forecast_errors_total",
		Help: "Count of forecast failures by backend.",
	}, []string{"backend"})

	// WebhookTotal counts Agones FleetAutoscaler webhook requests. A rising
	// `served="false"` rate means Agones is falling through to its Chain
	// fallback -- the intended failure mode, but one worth alerting on.
	WebhookTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "presage_agones_webhook_requests_total",
		Help: "Count of Agones FleetAutoscaler webhook requests by outcome.",
	}, []string{"namespace", "name", "served", "reason"})

	// SignalGaps reports how many steps of the last query were filled rather
	// than observed. A series that is mostly interpolation is not a series.
	SignalGaps = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "presage_signal_gap_steps",
		Help: "Number of steps in the last signal query that were gap-filled.",
	}, []string{"namespace", "name", "signal"})

	// CrossingQuantilesTotal counts how many times a signal's upper quantile
	// was below the lower quantile. A single occurrence is a data oddity;
	// repeated occurrences mean a backend is producing garbage and every
	// forecast should be treated as suspect.
	CrossingQuantilesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "presage_crossing_quantiles_total",
		Help: "Number of times quantile crossing was detected per signal.",
	}, []string{"namespace", "name", "signal"})
)

// Forget drops every series for a scaler, so a deleted PredictiveScaler stops
// exporting a stale recommendation forever.
func Forget(namespace, name, target string) {
	labels := prometheus.Labels{"namespace": namespace, "name": name, "target": target}
	RecommendedReplicas.Delete(labels)
	CurrentReplicas.Delete(labels)
	PredictiveReplicas.Delete(labels)
	ReactiveReplicas.Delete(labels)
	LeadTimeSeconds.Delete(labels)

	nn := prometheus.Labels{"namespace": namespace, "name": name}
	SignalValue.DeletePartialMatch(nn)
	SignalGaps.DeletePartialMatch(nn)
	ForecastValue.DeletePartialMatch(nn)
}

func init() {
	metrics.Registry.MustRegister(
		RecommendedReplicas, CurrentReplicas, PredictiveReplicas, ReactiveReplicas,
		ForecastValue, SignalValue, LeadTimeSeconds,
		ConstraintTotal, ReconcileTotal, ScaleTotal,
		ForecastDuration, ForecastErrorTotal, WebhookTotal, SignalGaps,
		CrossingQuantilesTotal,
	)
}
