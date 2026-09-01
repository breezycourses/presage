package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// Mode controls whether a PredictiveScaler actually writes to its target.
// +kubebuilder:validation:Enum=Shadow;Enforce
type Mode string

const (
	// ModeShadow computes and publishes a recommendation but never mutates the
	// target. This is the default, and is the intended way to evaluate a
	// PredictiveScaler against a workload before trusting it.
	ModeShadow Mode = "Shadow"
	// ModeEnforce applies the recommendation to the target.
	ModeEnforce Mode = "Enforce"
)

// ScaleTargetRef identifies the workload whose replica count is being managed.
//
// Two families of target are supported:
//
//   - Anything exposing the standard `scale` subresource (Deployment,
//     StatefulSet, ReplicaSet, and most custom resources that implement it).
//     This is the default and requires no target-specific support in presage.
//   - An Agones Fleet, which is NOT scaled directly. Instead presage serves a
//     FleetAutoscaler webhook for it; see AgonesFleetTarget.
type ScaleTargetRef struct {
	// APIVersion of the target, e.g. "apps/v1".
	// +kubebuilder:validation:MinLength=1
	APIVersion string `json:"apiVersion"`

	// Kind of the target, e.g. "Deployment".
	// +kubebuilder:validation:MinLength=1
	Kind string `json:"kind"`

	// Name of the target. Must be in the same namespace as the PredictiveScaler.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Agones carries options that only apply when Kind is "Fleet" in the
	// agones.dev group. When set, presage does not write replicas directly;
	// it publishes the recommendation on its FleetAutoscaler webhook endpoint
	// so that Agones remains the single writer of Fleet replicas.
	// +optional
	Agones *AgonesFleetTarget `json:"agones,omitempty"`
}

// AgonesFleetTarget configures the Agones FleetAutoscaler webhook adapter.
//
// Agones is deliberately left in charge of the actual write. presage exposes
// `/scale/<namespace>/<name>` and answers FleetAutoscaleReview requests from a
// cached recommendation. Pair it with a Chain policy so that a presage outage
// degrades to a plain Buffer policy rather than freezing the fleet:
//
//	policy:
//	  type: Chain
//	  chain:
//	    - {id: predictive, type: Webhook, webhook: {service: {...}}}
//	    - {id: fallback,   type: Buffer,  buffer: {bufferSize: 2, ...}}
type AgonesFleetTarget struct {
	// MaxRecommendationAge is how stale a cached recommendation may be before
	// the webhook starts refusing to answer (returning an error so that a
	// Chain policy falls through to its next entry). Keep this comfortably
	// above the reconcile Interval.
	// +kubebuilder:default="5m"
	// +optional
	MaxRecommendationAge *metav1.Duration `json:"maxRecommendationAge,omitempty"`
}

// NamedSignal is one demand dimension in a multi-signal scaler.
//
// Each signal is converted to a replica requirement independently and the
// largest binds, the way an HPA combines multiple metrics. A workload has to
// be big enough for every dimension it serves, so summing or averaging them
// would let a quiet dimension mask a busy one.
type NamedSignal struct {
	// Name identifies the signal in status and metrics. Must be unique within
	// the scaler.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Prometheus reads this signal from a Prometheus-compatible endpoint.
	Prometheus *PrometheusSignal `json:"prometheus"`

	// +kubebuilder:default="5m"
	// +optional
	Resolution *metav1.Duration `json:"resolution,omitempty"`

	// +kubebuilder:default="336h"
	// +optional
	History *metav1.Duration `json:"history,omitempty"`

	// Capacity converts this signal's units into replicas. Required, because
	// there is no sensible shared default across dimensions measured in
	// different units.
	Capacity CapacitySpec `json:"capacity"`
}

// SignalSpec describes the time series that drives the forecast.
type SignalSpec struct {
	// Prometheus reads the signal from a Prometheus-compatible range query
	// endpoint (Prometheus, Thanos, Mimir, VictoriaMetrics vmselect, ...).
	// +optional
	Prometheus *PrometheusSignal `json:"prometheus,omitempty"`

	// Resolution is the bucket width the series is sampled at. This is the
	// single most consequential tuning knob: a foundation model sees a fixed
	// number of points, so resolution decides how far back its context reaches.
	// At 5m with a 16k-point context you get ~57 days, which comfortably covers
	// weekly seasonality. At 1m you get ~11 days, which does not.
	// +kubebuilder:default="5m"
	// +optional
	Resolution *metav1.Duration `json:"resolution,omitempty"`

	// History is how much past data to feed the model. Capped by the backend's
	// maximum context length. Two to four weeks is a reasonable default for
	// workloads with weekly seasonality.
	// +kubebuilder:default="336h"
	// +optional
	History *metav1.Duration `json:"history,omitempty"`
}

// PrometheusSignal is a Prometheus range query producing a single series.
type PrometheusSignal struct {
	// Address is the base URL of the query endpoint, e.g.
	// "http://vmselect.monitoring:8481/select/0/prometheus".
	// +kubebuilder:validation:MinLength=1
	Address string `json:"address"`

	// Query must evaluate to exactly one series. If it returns more than one,
	// the PredictiveScaler goes Degraded rather than silently picking one.
	// +kubebuilder:validation:MinLength=1
	Query string `json:"query"`

	// BearerTokenSecretRef optionally supplies an Authorization bearer token.
	// +optional
	BearerTokenSecretRef *SecretKeySelector `json:"bearerTokenSecretRef,omitempty"`

	// InsecureSkipVerify disables TLS verification. Not recommended.
	// +optional
	InsecureSkipVerify bool `json:"insecureSkipVerify,omitempty"`
}

// SecretKeySelector selects one key out of one Secret in the scaler's namespace.
type SecretKeySelector struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`
}

// LeadTimeSource selects how the provisioning lead time is determined.
// +kubebuilder:validation:Enum=Static;Observed
type LeadTimeSource string

const (
	// LeadTimeStatic uses a fixed configured duration.
	LeadTimeStatic LeadTimeSource = "Static"
	// LeadTimeObserved reads the lead time from a Prometheus query, so it
	// tracks reality as image sizes, node pressure, and startup work change.
	LeadTimeObserved LeadTimeSource = "Observed"
)

// LeadTimeSpec configures the forecast horizon.
//
// This is the reason predictive autoscaling beats reactive autoscaling. A
// reactive autoscaler observes demand at time T and starts a replica that is
// only useful at T+lead, so it is structurally always one lead time behind.
// presage forecasts demand *at* T+lead and provisions for that instead.
type LeadTimeSpec struct {
	// +kubebuilder:default=Static
	// +optional
	Source LeadTimeSource `json:"source,omitempty"`

	// Static is the lead time used when Source is Static.
	// +kubebuilder:default="2m"
	// +optional
	Static *metav1.Duration `json:"static,omitempty"`

	// Observed is the query used when Source is Observed. It must return a
	// single scalar in seconds -- typically a high quantile of the time from
	// pod creation to readiness.
	// +optional
	Observed *PrometheusSignal `json:"observed,omitempty"`

	// Min and Max clamp the resolved lead time. They bound the damage from a
	// bad Observed query and stop the horizon from collapsing to zero (which
	// would silently turn presage into a reactive autoscaler).
	// +kubebuilder:default="30s"
	// +optional
	Min *metav1.Duration `json:"min,omitempty"`
	// +kubebuilder:default="15m"
	// +optional
	Max *metav1.Duration `json:"max,omitempty"`
}

// CapacitySpec converts forecast units into replicas.
type CapacitySpec struct {
	// PerReplica is how many units of the signal one replica serves. If the
	// signal is already expressed in replicas (for example, forecasting
	// allocated GameServers directly), set this to 1.
	// +kubebuilder:default="1"
	// +optional
	PerReplica *resource.Quantity `json:"perReplica,omitempty"`

	// Query optionally derives per-replica capacity from live data instead of
	// a constant, e.g. average capacity across the fleet. Takes precedence
	// over PerReplica when set.
	// +optional
	Query *PrometheusSignal `json:"query,omitempty"`
}

// PolicySpec is the decision layer: how a predictive distribution becomes a
// replica count.
type PolicySpec struct {
	// TargetQuantile is the service level capacity is sized to: the workload
	// is provisioned for the demand this quantile of the forecast implies, in
	// both directions. Raising it buys protection against being wrong in the
	// expensive direction.
	//
	// Note that a more uncertain forecast has a fatter upper tail, so this
	// automatically provisions more when the model is unsure. Uncertainty
	// should buy capacity, not hesitation.
	// +kubebuilder:default="0.9"
	// +optional
	TargetQuantile string `json:"targetQuantile,omitempty"`

	// ScaleDownQuantile is the lower quantile used to measure how confident
	// the forecast is. It never sets the target -- its only job is to feed
	// ScaleDownUncertaintyGuard.
	// +kubebuilder:default="0.5"
	// +optional
	ScaleDownQuantile string `json:"scaleDownQuantile,omitempty"`

	// ScaleDownUncertaintyGuard refuses to release capacity while the forecast
	// is too uncertain. Adding capacity is deliberately never gated this way.
	// +optional
	ScaleDownUncertaintyGuard *UncertaintyGuardSpec `json:"scaleDownUncertaintyGuard,omitempty"`

	// Headroom is extra capacity applied on top of the forecast, as an
	// absolute number of signal units or a percentage.
	// +kubebuilder:default="10%"
	// +optional
	Headroom *intstr.IntOrString `json:"headroom,omitempty"`

	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=1
	// +optional
	MinReplicas int32 `json:"minReplicas,omitempty"`

	// +kubebuilder:validation:Minimum=1
	MaxReplicas int32 `json:"maxReplicas"`

	// ReactiveFloor keeps a conventional reactive computation running
	// alongside the forecast and takes the maximum of the two. With it
	// enabled, forecast error can only ever cause over-provisioning -- presage
	// is then strictly safer than the reactive policy it replaces. Disabling
	// it is what unlocks scale-to-zero, at the cost of that guarantee.
	//
	// MaxReplicas and MaxScaleUpRate remain hard constraints and can bind
	// below the floor; the floor removes forecast error as a cause of
	// under-provisioning, not operator-configured limits.
	// +optional
	ReactiveFloor *ReactiveFloorSpec `json:"reactiveFloor,omitempty"`

	// +optional
	Stabilization *StabilizationSpec `json:"stabilization,omitempty"`
}

// UncertaintyGuardSpec blocks scale-downs on an unconfident forecast.
type UncertaintyGuardSpec struct {
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// MaxRelativeSpread is the largest (upper - lower) / max(lower, 1) that
	// still permits a scale-down. Lower values are more conservative.
	// +kubebuilder:default="0.25"
	// +optional
	MaxRelativeSpread string `json:"maxRelativeSpread,omitempty"`
}

// ReactiveFloorSpec mirrors a classic buffer autoscaler and is used as a lower
// bound on the recommendation.
type ReactiveFloorSpec struct {
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// BufferSize is the spare capacity the reactive floor insists on, as an
	// absolute number of replicas or a percentage of current demand.
	// +kubebuilder:default="10%"
	// +optional
	BufferSize *intstr.IntOrString `json:"bufferSize,omitempty"`
}

// StabilizationSpec damps oscillation.
type StabilizationSpec struct {
	// ScaleDownWindow is how long the recommendation must stay below the
	// current replica count before a scale-down is allowed. Scale-up is
	// deliberately not delayed by default: the whole point is to be early.
	// +kubebuilder:default="15m"
	// +optional
	ScaleDownWindow *metav1.Duration `json:"scaleDownWindow,omitempty"`

	// MaxScaleUpRate caps a single step's increase, as replicas or a
	// percentage of current replicas.
	// +kubebuilder:default="100%"
	// +optional
	MaxScaleUpRate *intstr.IntOrString `json:"maxScaleUpRate,omitempty"`

	// MaxScaleDownRate caps a single step's decrease.
	// +kubebuilder:default="20%"
	// +optional
	MaxScaleDownRate *intstr.IntOrString `json:"maxScaleDownRate,omitempty"`
}

// ForecastSpec selects and parameterises the forecasting backend.
type ForecastSpec struct {
	// BackendRef names a cluster-scoped ForecastBackend. If unset, the backend
	// named "default" is used.
	// +optional
	BackendRef *string `json:"backendRef,omitempty"`

	// Horizon is how far ahead to forecast. It must be at least the lead time;
	// forecasting further is harmless and makes the projected curve useful for
	// dashboards. Defaults to 4x the resolved lead time.
	// +optional
	Horizon *metav1.Duration `json:"horizon,omitempty"`
}

// PredictiveScalerSpec defines the desired state of a PredictiveScaler.
type PredictiveScalerSpec struct {
	ScaleTargetRef ScaleTargetRef `json:"scaleTargetRef"`
	Policy         PolicySpec     `json:"policy"`

	// Signal is the single-signal form, paired with the top-level Capacity.
	// Exactly one of Signal or Signals must be set.
	// +optional
	Signal *SignalSpec `json:"signal,omitempty"`

	// Signals is the multi-signal form. Each entry becomes a replica
	// requirement and the largest binds, so the workload ends up sized for
	// whichever dimension needs the most.
	// +optional
	// +listType=map
	// +listMapKey=name
	Signals []NamedSignal `json:"signals,omitempty"`

	// +optional
	LeadTime *LeadTimeSpec `json:"leadTime,omitempty"`
	// Capacity applies to the single-signal form only; multi-signal scalers
	// carry a capacity per signal.
	// +optional
	Capacity *CapacitySpec `json:"capacity,omitempty"`
	// +optional
	Forecast *ForecastSpec `json:"forecast,omitempty"`

	// Mode defaults to Shadow. Run a workload in Shadow long enough to compare
	// the recommendation against what actually happened before switching.
	// +kubebuilder:default=Shadow
	// +optional
	Mode Mode `json:"mode,omitempty"`

	// Interval is how often to refresh the forecast and recommendation. Note
	// that for Agones targets this is decoupled from the FleetAutoscaler sync
	// period: Agones may poll the webhook every 30s while presage refreshes
	// the underlying forecast far less often.
	// +kubebuilder:default="1m"
	// +optional
	Interval *metav1.Duration `json:"interval,omitempty"`
}

// ForecastSample is the forecast evaluated at the lead time.
type ForecastSample struct {
	// GeneratedAt is when the forecast was produced.
	GeneratedAt metav1.Time `json:"generatedAt"`
	// Backend that produced it.
	Backend string `json:"backend"`
	// Model identifier reported by the backend, if any.
	// +optional
	Model string `json:"model,omitempty"`
	// LeadTimeSeconds actually used for this forecast.
	LeadTimeSeconds int32 `json:"leadTimeSeconds"`
	// Point is the point forecast at the lead time.
	Point string `json:"point"`
	// Quantiles maps quantile to forecast value at the lead time.
	// +optional
	Quantiles map[string]string `json:"quantiles,omitempty"`
}

// SignalStatus is the per-signal view of one evaluation.
type SignalStatus struct {
	Name string `json:"name"`
	// Replicas this signal alone would have required.
	Replicas int32 `json:"replicas"`
	// Observed is the latest sampled value.
	Observed string `json:"observed"`
	// Forecast at the lead time, at the target quantile.
	Forecast string `json:"forecast"`
	// GapSteps is how many steps of the query window were gap-filled.
	// +optional
	GapSteps int32 `json:"gapSteps,omitempty"`
}

// RecommendationBreakdown records how the number was arrived at, so that a
// surprising replica count can be explained without re-deriving it.
type RecommendationBreakdown struct {
	// Predictive is the replica count implied by the forecast alone.
	Predictive int32 `json:"predictive"`
	// Reactive is the replica count the reactive floor would have chosen.
	// +optional
	Reactive *int32 `json:"reactive,omitempty"`
	// Constraint names the binding constraint, if any: one of
	// "MinReplicas", "MaxReplicas", "ReactiveFloor", "ForecastUncertainty",
	// "ScaleDownWindow", "MaxScaleUpRate", "MaxScaleDownRate", or "" when the
	// forecast bound.
	// +optional
	Constraint string `json:"constraint,omitempty"`

	// BindingSignal names the signal that required the most replicas. With
	// several signals this answers "which dimension is driving the size of
	// this workload", which is the first thing anyone asks.
	// +optional
	BindingSignal string `json:"bindingSignal,omitempty"`
}

// PredictiveScalerStatus is the observed state of a PredictiveScaler.
type PredictiveScalerStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// CurrentReplicas last read from the target.
	// +optional
	CurrentReplicas int32 `json:"currentReplicas,omitempty"`
	// RecommendedReplicas is what presage would apply (and does apply in
	// Enforce mode).
	// +optional
	RecommendedReplicas int32 `json:"recommendedReplicas,omitempty"`
	// +optional
	Breakdown *RecommendationBreakdown `json:"breakdown,omitempty"`
	// LastForecast describes the binding signal's forecast.
	// +optional
	LastForecast *ForecastSample `json:"lastForecast,omitempty"`

	// SignalStatuses carries one entry per configured signal, so a
	// multi-signal scaler can be debugged without guessing which dimension
	// produced which number.
	// +optional
	// +listType=map
	// +listMapKey=name
	SignalStatuses []SignalStatus `json:"signalStatuses,omitempty"`
	// +optional
	LastScaleTime *metav1.Time `json:"lastScaleTime,omitempty"`
	// ScaleDownCandidateSince tracks the start of the scale-down stabilization
	// window. Cleared whenever the recommendation stops being below current.
	// +optional
	ScaleDownCandidateSince *metav1.Time `json:"scaleDownCandidateSince,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Condition types set on a PredictiveScaler.
const (
	// ConditionReady is true when the scaler has a fresh, usable recommendation.
	ConditionReady = "Ready"
	// ConditionForecasting is true when the backend last returned a forecast.
	ConditionForecasting = "Forecasting"
	// ConditionScalingActive is true when the scaler is in Enforce mode and
	// permitted to write to its target.
	ConditionScalingActive = "ScalingActive"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=pscaler;ps,categories=autoscaling
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.scaleTargetRef.name`
// +kubebuilder:printcolumn:name="Current",type=integer,JSONPath=`.status.currentReplicas`
// +kubebuilder:printcolumn:name="Recommended",type=integer,JSONPath=`.status.recommendedReplicas`
// +kubebuilder:printcolumn:name="Bound",type=string,priority=1,JSONPath=`.status.breakdown.constraint`
// +kubebuilder:printcolumn:name="Signal",type=string,priority=1,JSONPath=`.status.breakdown.bindingSignal`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PredictiveScaler forecasts a workload's demand and sizes it for the demand
// expected once new replicas are actually ready.
type PredictiveScaler struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PredictiveScalerSpec   `json:"spec,omitempty"`
	Status PredictiveScalerStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PredictiveScalerList contains a list of PredictiveScaler.
type PredictiveScalerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PredictiveScaler `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PredictiveScaler{}, &PredictiveScalerList{})
}
