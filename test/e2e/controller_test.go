package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/breezycourses/presage/api/v1alpha1"
	"github.com/breezycourses/presage/internal/agones"
	"github.com/breezycourses/presage/internal/scaletarget"
)

const (
	// A short season keeps the suite fast: 12 steps of 5m is one hour.
	seasonSteps = 12
	resolution  = 5 * time.Minute
	season      = time.Duration(seasonSteps) * resolution
	history     = 6 * time.Hour
)

// eventually polls until cond returns nil or the deadline passes, reporting
// the last error. Controller-runtime work is asynchronous; asserting once
// immediately after a Create would be a race.
func eventually(t *testing.T, timeout time.Duration, cond func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		if last = cond(); last == nil {
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("condition never met within %s: %v", timeout, last)
}

// consistently asserts cond holds for the whole window. Used for negative
// claims -- "it did not scale" -- which a single check cannot establish.
func consistently(t *testing.T, window time.Duration, cond func() error) {
	t.Helper()
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		if err := cond(); err != nil {
			t.Fatalf("condition stopped holding: %v", err)
		}
		time.Sleep(150 * time.Millisecond)
	}
}

func newNamespace(t *testing.T) string {
	t.Helper()
	name := fmt.Sprintf("e2e-%d", time.Now().UnixNano()%1_000_000_000)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	return name
}

func ensureBackend(t *testing.T, name string) {
	t.Helper()
	fb := &v1alpha1.ForecastBackend{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.ForecastBackendSpec{
			Type: v1alpha1.BackendSeasonalNaive,
			SeasonalNaive: &v1alpha1.SeasonalNaiveBackend{
				Season: &metav1.Duration{Duration: season},
				Cycles: 2,
			},
		},
	}
	err := k8sClient.Create(context.Background(), fb)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create ForecastBackend: %v", err)
	}
}

func newDeployment(t *testing.T, ns, name string, replicas int32) *appsv1.Deployment {
	t.Helper()
	labels := map[string]string{"app": name}
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "app", Image: "nginx"}},
				},
			},
		},
	}
	if err := k8sClient.Create(context.Background(), d); err != nil {
		t.Fatalf("create Deployment: %v", err)
	}
	return d
}

func baseScaler(ns, name string, target v1alpha1.ScaleTargetRef) *v1alpha1.PredictiveScaler {
	perReplica := resourceQuantity("10")
	return &v1alpha1.PredictiveScaler{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v1alpha1.PredictiveScalerSpec{
			Mode:           v1alpha1.ModeShadow,
			Interval:       &metav1.Duration{Duration: time.Second},
			ScaleTargetRef: target,
			Signal: &v1alpha1.SignalSpec{
				Prometheus: &v1alpha1.PrometheusSignal{
					Address: prom.URL,
					Query:   "sum(demand)",
				},
				Resolution: &metav1.Duration{Duration: resolution},
				History:    &metav1.Duration{Duration: history},
			},
			LeadTime: &v1alpha1.LeadTimeSpec{
				Source: v1alpha1.LeadTimeStatic,
				Static: &metav1.Duration{Duration: 10 * time.Minute},
			},
			Capacity: &v1alpha1.CapacitySpec{PerReplica: &perReplica},
			Forecast: &v1alpha1.ForecastSpec{BackendRef: strPtr("default")},
			Policy: v1alpha1.PolicySpec{
				MinReplicas:    1,
				MaxReplicas:    100,
				TargetQuantile: "0.9",
				Headroom:       intOrStrPtr(intstr.FromInt32(0)),
				ReactiveFloor:  &v1alpha1.ReactiveFloorSpec{Enabled: boolPtr(false)},
				Stabilization: &v1alpha1.StabilizationSpec{
					ScaleDownWindow:  &metav1.Duration{Duration: 0},
					MaxScaleUpRate:   intOrStrPtr(intstr.FromString("10000%")),
					MaxScaleDownRate: intOrStrPtr(intstr.FromString("10000%")),
				},
				ScaleDownUncertaintyGuard: &v1alpha1.UncertaintyGuardSpec{Enabled: boolPtr(false)},
			},
		},
	}
}

func getScaler(t *testing.T, ns, name string) *v1alpha1.PredictiveScaler {
	t.Helper()
	var ps v1alpha1.PredictiveScaler
	if err := k8sClient.Get(context.Background(),
		types.NamespacedName{Namespace: ns, Name: name}, &ps); err != nil {
		t.Fatalf("get PredictiveScaler: %v", err)
	}
	return &ps
}

func conditionStatus(ps *v1alpha1.PredictiveScaler, condType string) (metav1.ConditionStatus, string) {
	for _, c := range ps.Status.Conditions {
		if c.Type == condType {
			return c.Status, c.Reason
		}
	}
	return "", ""
}

// ---------------------------------------------------------------------------
// tests
// ---------------------------------------------------------------------------

// TestShadowModePublishesButDoesNotScale is the adoption path: presage must be
// evaluable without being trusted first.
func TestShadowModePublishesButDoesNotScale(t *testing.T) {
	ns := newNamespace(t)
	ensureBackend(t, "default")
	newDeployment(t, ns, "api", 3)

	prom.setSeries(constantSeries(100)) // 100 demand / 10 per replica = 10

	ps := baseScaler(ns, "api", v1alpha1.ScaleTargetRef{
		APIVersion: "apps/v1", Kind: "Deployment", Name: "api",
	})
	if err := k8sClient.Create(context.Background(), ps); err != nil {
		t.Fatalf("create PredictiveScaler: %v", err)
	}

	eventually(t, 30*time.Second, func() error {
		got := getScaler(t, ns, "api")
		if got.Status.RecommendedReplicas != 10 {
			return fmt.Errorf("recommendedReplicas = %d, want 10", got.Status.RecommendedReplicas)
		}
		if got.Status.CurrentReplicas != 3 {
			return fmt.Errorf("currentReplicas = %d, want 3", got.Status.CurrentReplicas)
		}
		if status, _ := conditionStatus(got, v1alpha1.ConditionReady); status != metav1.ConditionTrue {
			return fmt.Errorf("Ready = %q, want True", status)
		}
		if status, reason := conditionStatus(got, v1alpha1.ConditionScalingActive); status != metav1.ConditionFalse || reason != "ShadowMode" {
			return fmt.Errorf("ScalingActive = %q/%q, want False/ShadowMode", status, reason)
		}
		if got.Status.LastForecast == nil || got.Status.LastForecast.Backend != "SeasonalNaive" {
			return fmt.Errorf("lastForecast not populated: %+v", got.Status.LastForecast)
		}
		return nil
	})

	// The whole point of Shadow: the Deployment must not move.
	consistently(t, 3*time.Second, func() error {
		var d appsv1.Deployment
		if err := k8sClient.Get(context.Background(),
			types.NamespacedName{Namespace: ns, Name: "api"}, &d); err != nil {
			return err
		}
		if *d.Spec.Replicas != 3 {
			return fmt.Errorf("Shadow mode scaled the Deployment to %d", *d.Spec.Replicas)
		}
		return nil
	})
}

// TestEnforceModeScalesTheDeployment exercises the real scale subresource.
func TestEnforceModeScalesTheDeployment(t *testing.T) {
	ns := newNamespace(t)
	ensureBackend(t, "default")
	newDeployment(t, ns, "api", 3)

	prom.setSeries(constantSeries(70)) // 7 replicas

	ps := baseScaler(ns, "api", v1alpha1.ScaleTargetRef{
		APIVersion: "apps/v1", Kind: "Deployment", Name: "api",
	})
	ps.Spec.Mode = v1alpha1.ModeEnforce
	if err := k8sClient.Create(context.Background(), ps); err != nil {
		t.Fatalf("create PredictiveScaler: %v", err)
	}

	eventually(t, 30*time.Second, func() error {
		var d appsv1.Deployment
		if err := k8sClient.Get(context.Background(),
			types.NamespacedName{Namespace: ns, Name: "api"}, &d); err != nil {
			return err
		}
		if *d.Spec.Replicas != 7 {
			return fmt.Errorf("replicas = %d, want 7", *d.Spec.Replicas)
		}
		return nil
	})

	eventually(t, 10*time.Second, func() error {
		got := getScaler(t, ns, "api")
		if status, _ := conditionStatus(got, v1alpha1.ConditionScalingActive); status != metav1.ConditionTrue {
			return fmt.Errorf("ScalingActive = %q, want True", status)
		}
		if got.Status.LastScaleTime == nil {
			return fmt.Errorf("lastScaleTime not recorded")
		}
		return nil
	})
}

// TestReactiveFloorPreventsUnderProvisioning is the headline safety property,
// end to end: the forecast is genuinely, badly wrong -- a season ago was quiet,
// now is busy -- and the workload must still be sized for present demand.
func TestReactiveFloorPreventsUnderProvisioning(t *testing.T) {
	ns := newNamespace(t)
	ensureBackend(t, "default")
	newDeployment(t, ns, "api", 5)

	// Season ago: 20 (2 replicas). Now: 300 (30 replicas).
	prom.setSeries(quietPastBusyNow(seasonSteps, 20, 300))

	ps := baseScaler(ns, "api", v1alpha1.ScaleTargetRef{
		APIVersion: "apps/v1", Kind: "Deployment", Name: "api",
	})
	ps.Spec.Mode = v1alpha1.ModeEnforce
	ps.Spec.Policy.ReactiveFloor = &v1alpha1.ReactiveFloorSpec{
		Enabled:    boolPtr(true),
		BufferSize: intOrStrPtr(intstr.FromInt32(2)),
	}
	if err := k8sClient.Create(context.Background(), ps); err != nil {
		t.Fatalf("create PredictiveScaler: %v", err)
	}

	eventually(t, 30*time.Second, func() error {
		got := getScaler(t, ns, "api")
		if got.Status.Breakdown == nil {
			return fmt.Errorf("breakdown not populated")
		}
		// Present demand of 300 at 10/replica is 30, plus a 2-replica buffer.
		if got.Status.RecommendedReplicas != 32 {
			return fmt.Errorf("recommendedReplicas = %d, want 32 (floor)", got.Status.RecommendedReplicas)
		}
		if got.Status.Breakdown.Constraint != "ReactiveFloor" {
			return fmt.Errorf("constraint = %q, want ReactiveFloor", got.Status.Breakdown.Constraint)
		}
		// The forecast's own (wrong, low) opinion must still be reported, so
		// the disagreement is visible rather than hidden by the floor.
		if got.Status.Breakdown.Predictive >= 32 {
			return fmt.Errorf("predictive = %d, expected it to be well below the floor",
				got.Status.Breakdown.Predictive)
		}
		return nil
	})
}

// TestAgonesFleetServesWebhookFromRecommendation covers the flagship path
// without Agones installed: a Fleet-shaped resource, the reconciler publishing
// to the store, and the webhook handler answering a real FleetAutoscaleReview.
func TestAgonesFleetServesWebhookFromRecommendation(t *testing.T) {
	ns := newNamespace(t)
	ensureBackend(t, "default")
	createFleet(t, ns, "lobby", 4)

	prom.setSeries(constantSeries(90)) // 9 replicas

	ps := baseScaler(ns, "lobby", v1alpha1.ScaleTargetRef{
		APIVersion: "agones.dev/v1", Kind: "Fleet", Name: "lobby",
	})
	ps.Spec.Mode = v1alpha1.ModeEnforce
	if err := k8sClient.Create(context.Background(), ps); err != nil {
		t.Fatalf("create PredictiveScaler: %v", err)
	}

	eventually(t, 30*time.Second, func() error {
		got := getScaler(t, ns, "lobby")
		if got.Status.RecommendedReplicas != 9 {
			return fmt.Errorf("recommendedReplicas = %d, want 9", got.Status.RecommendedReplicas)
		}
		// The Fleet's own replica count comes from status, which presage reads
		// but never writes.
		if got.Status.CurrentReplicas != 4 {
			return fmt.Errorf("currentReplicas = %d, want 4 (from Fleet status)", got.Status.CurrentReplicas)
		}
		return nil
	})

	// presage must not have written to the Fleet: Agones stays the sole writer.
	fleet := getFleet(t, ns, "lobby")
	if replicas, found, _ := unstructured.NestedInt64(fleet.Object, "spec", "replicas"); found && replicas != 4 {
		t.Fatalf("presage wrote spec.replicas = %d; Agones must remain the only writer", replicas)
	}

	// And the webhook must serve the recommendation Agones asks for.
	resp := postReview(t, ns, "lobby")
	if resp.Code != http.StatusOK {
		t.Fatalf("webhook returned %d: %s", resp.Code, resp.Body.String())
	}
	var review agones.FleetAutoscaleReview
	if err := json.Unmarshal(resp.Body.Bytes(), &review); err != nil {
		t.Fatalf("undecodable review: %v", err)
	}
	if review.Response == nil || !review.Response.Scale || review.Response.Replicas != 9 {
		t.Fatalf("unexpected webhook response: %+v", review.Response)
	}
	if review.Response.UID != "review-uid" {
		t.Fatalf("UID not echoed: %q", review.Response.UID)
	}
}

// TestAgonesShadowModeFallsThroughToChain: in Shadow the webhook must refuse,
// so an Agones Chain policy uses its fallback rather than acting on an
// advisory number.
func TestAgonesShadowModeFallsThroughToChain(t *testing.T) {
	ns := newNamespace(t)
	ensureBackend(t, "default")
	createFleet(t, ns, "lobby", 4)

	prom.setSeries(constantSeries(90))

	ps := baseScaler(ns, "lobby", v1alpha1.ScaleTargetRef{
		APIVersion: "agones.dev/v1", Kind: "Fleet", Name: "lobby",
	}) // Shadow by default
	if err := k8sClient.Create(context.Background(), ps); err != nil {
		t.Fatalf("create PredictiveScaler: %v", err)
	}

	eventually(t, 30*time.Second, func() error {
		if getScaler(t, ns, "lobby").Status.RecommendedReplicas != 9 {
			return fmt.Errorf("waiting for a recommendation")
		}
		return nil
	})

	resp := postReview(t, ns, "lobby")
	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("Shadow mode must refuse to scale, got %d: %s", resp.Code, resp.Body.String())
	}
	if bytes.Contains(resp.Body.Bytes(), []byte(`"scale"`)) {
		t.Fatalf("must not return a well-formed review in Shadow mode: %s", resp.Body.String())
	}
}

// TestDeletionStopsServingTheWebhook: a deleted scaler must not keep answering
// until its recommendation happens to go stale.
func TestDeletionStopsServingTheWebhook(t *testing.T) {
	ns := newNamespace(t)
	ensureBackend(t, "default")
	createFleet(t, ns, "lobby", 4)
	prom.setSeries(constantSeries(90))

	ps := baseScaler(ns, "lobby", v1alpha1.ScaleTargetRef{
		APIVersion: "agones.dev/v1", Kind: "Fleet", Name: "lobby",
	})
	ps.Spec.Mode = v1alpha1.ModeEnforce
	if err := k8sClient.Create(context.Background(), ps); err != nil {
		t.Fatalf("create PredictiveScaler: %v", err)
	}

	eventually(t, 30*time.Second, func() error {
		if postReview(t, ns, "lobby").Code != http.StatusOK {
			return fmt.Errorf("waiting for the webhook to serve")
		}
		return nil
	})

	if err := k8sClient.Delete(context.Background(), getScaler(t, ns, "lobby")); err != nil {
		t.Fatalf("delete PredictiveScaler: %v", err)
	}

	eventually(t, 20*time.Second, func() error {
		if code := postReview(t, ns, "lobby").Code; code != http.StatusServiceUnavailable {
			return fmt.Errorf("webhook still serving after delete: %d", code)
		}
		return nil
	})

	// The finalizer must actually be released, or the object leaks forever.
	eventually(t, 20*time.Second, func() error {
		var got v1alpha1.PredictiveScaler
		err := k8sClient.Get(context.Background(),
			types.NamespacedName{Namespace: ns, Name: "lobby"}, &got)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		return fmt.Errorf("object still present with finalizers %v", got.Finalizers)
	})
}

// TestMissingBackendSurfacesInStatus: a misconfiguration must be visible on
// the object, not only in controller logs.
func TestMissingBackendSurfacesInStatus(t *testing.T) {
	ns := newNamespace(t)
	newDeployment(t, ns, "api", 3)
	prom.setSeries(constantSeries(100))

	ps := baseScaler(ns, "api", v1alpha1.ScaleTargetRef{
		APIVersion: "apps/v1", Kind: "Deployment", Name: "api",
	})
	ps.Spec.Forecast = &v1alpha1.ForecastSpec{BackendRef: strPtr("does-not-exist")}
	if err := k8sClient.Create(context.Background(), ps); err != nil {
		t.Fatalf("create PredictiveScaler: %v", err)
	}

	eventually(t, 30*time.Second, func() error {
		got := getScaler(t, ns, "api")
		status, reason := conditionStatus(got, v1alpha1.ConditionReady)
		if status != metav1.ConditionFalse {
			return fmt.Errorf("Ready = %q, want False", status)
		}
		if reason != "EvaluationFailed" {
			return fmt.Errorf("reason = %q, want EvaluationFailed", reason)
		}
		return nil
	})

	// A broken scaler must not leave the Deployment mid-flight either.
	consistently(t, 2*time.Second, func() error {
		var d appsv1.Deployment
		if err := k8sClient.Get(context.Background(),
			types.NamespacedName{Namespace: ns, Name: "api"}, &d); err != nil {
			return err
		}
		if *d.Spec.Replicas != 3 {
			return fmt.Errorf("a failing scaler scaled the Deployment to %d", *d.Spec.Replicas)
		}
		return nil
	})
}

// TestGappySignalIsRejected: a series that is mostly interpolation must not be
// treated as evidence.
func TestGappySignalIsRejected(t *testing.T) {
	ns := newNamespace(t)
	ensureBackend(t, "default")
	newDeployment(t, ns, "api", 3)

	prom.setSeries(constantSeries(100))
	prom.setDropEvery(3) // keep 1 sample in 3: two thirds of the window is gaps
	t.Cleanup(func() { prom.setDropEvery(0) })

	ps := baseScaler(ns, "api", v1alpha1.ScaleTargetRef{
		APIVersion: "apps/v1", Kind: "Deployment", Name: "api",
	})
	ps.Spec.Mode = v1alpha1.ModeEnforce
	if err := k8sClient.Create(context.Background(), ps); err != nil {
		t.Fatalf("create PredictiveScaler: %v", err)
	}

	eventually(t, 30*time.Second, func() error {
		got := getScaler(t, ns, "api")
		status, reason := conditionStatus(got, v1alpha1.ConditionReady)
		if status != metav1.ConditionFalse || reason != "EvaluationFailed" {
			return fmt.Errorf("Ready = %q/%q, want False/EvaluationFailed", status, reason)
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func createFleet(t *testing.T, ns, name string, replicas int64) {
	t.Helper()
	fleet := &unstructured.Unstructured{}
	fleet.SetGroupVersionKind(scaletarget.AgonesFleetGVK)
	fleet.SetNamespace(ns)
	fleet.SetName(name)
	must(unstructured.SetNestedField(fleet.Object, replicas, "spec", "replicas"))
	if err := k8sClient.Create(context.Background(), fleet); err != nil {
		t.Fatalf("create Fleet: %v", err)
	}

	// Populate status separately: it is a subresource.
	must(unstructured.SetNestedField(fleet.Object, replicas, "status", "replicas"))
	must(unstructured.SetNestedField(fleet.Object, replicas, "status", "readyReplicas"))
	if err := k8sClient.Status().Update(context.Background(), fleet); err != nil {
		t.Fatalf("update Fleet status: %v", err)
	}
}

func getFleet(t *testing.T, ns, name string) *unstructured.Unstructured {
	t.Helper()
	fleet := &unstructured.Unstructured{}
	fleet.SetGroupVersionKind(scaletarget.AgonesFleetGVK)
	if err := k8sClient.Get(context.Background(),
		types.NamespacedName{Namespace: ns, Name: name}, fleet); err != nil {
		t.Fatalf("get Fleet: %v", err)
	}
	return fleet
}

// postReview drives the real webhook handler with a real FleetAutoscaleReview,
// against the same store the reconciler writes to.
func postReview(t *testing.T, ns, name string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(agones.FleetAutoscaleReview{
		Request: &agones.FleetAutoscaleRequest{
			UID:       "review-uid",
			Name:      name,
			Namespace: ns,
			Status:    agones.FleetStatus{Replicas: 4, ReadyReplicas: 4},
		},
	})
	if err != nil {
		t.Fatalf("marshal review: %v", err)
	}

	handler := &agones.Handler{Store: agonesStore}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/scale/%s/%s", ns, name), bytes.NewReader(body)))
	return rec
}

func strPtr(s string) *string                              { return &s }
func boolPtr(b bool) *bool                                 { return &b }
func intOrStrPtr(v intstr.IntOrString) *intstr.IntOrString { return &v }

var _ = client.ObjectKey{}

// TestMultipleSignalsTakeTheLargest: a workload has to be big enough for every
// dimension it serves, so the binding signal is whichever needs the most.
func TestMultipleSignalsTakeTheLargest(t *testing.T) {
	ns := newNamespace(t)
	ensureBackend(t, "default")
	newDeployment(t, ns, "api", 2)

	prom.setSeries(constantSeries(100))

	ps := baseScaler(ns, "api", v1alpha1.ScaleTargetRef{
		APIVersion: "apps/v1", Kind: "Deployment", Name: "api",
	})
	ps.Spec.Mode = v1alpha1.ModeEnforce
	// Same underlying series, different capacities: "cpu" needs 20 replicas,
	// "players" needs 10, so the recommendation must be 20.
	ps.Spec.Signal = nil
	ps.Spec.Capacity = nil
	ps.Spec.Signals = []v1alpha1.NamedSignal{
		{
			Name:       "players",
			Prometheus: &v1alpha1.PrometheusSignal{Address: prom.URL, Query: "sum(players)"},
			Resolution: &metav1.Duration{Duration: resolution},
			History:    &metav1.Duration{Duration: history},
			Capacity:   v1alpha1.CapacitySpec{PerReplica: quantityPtr("10")},
		},
		{
			Name:       "cpu",
			Prometheus: &v1alpha1.PrometheusSignal{Address: prom.URL, Query: "sum(cpu)"},
			Resolution: &metav1.Duration{Duration: resolution},
			History:    &metav1.Duration{Duration: history},
			Capacity:   v1alpha1.CapacitySpec{PerReplica: quantityPtr("5")},
		},
	}
	if err := k8sClient.Create(context.Background(), ps); err != nil {
		t.Fatalf("create PredictiveScaler: %v", err)
	}

	eventually(t, 30*time.Second, func() error {
		got := getScaler(t, ns, "api")
		if got.Status.RecommendedReplicas != 20 {
			return fmt.Errorf("recommendedReplicas = %d, want 20 (the cpu signal)",
				got.Status.RecommendedReplicas)
		}
		if got.Status.Breakdown == nil || got.Status.Breakdown.BindingSignal != "cpu" {
			return fmt.Errorf("bindingSignal = %v, want cpu", got.Status.Breakdown)
		}
		if len(got.Status.SignalStatuses) != 2 {
			return fmt.Errorf("expected per-signal status for both signals, got %d",
				len(got.Status.SignalStatuses))
		}
		return nil
	})
}

// TestBothSignalFormsIsRejected: the two spec shapes are alternatives, and
// silently preferring one would make the other look ignored.
func TestBothSignalFormsIsRejected(t *testing.T) {
	ns := newNamespace(t)
	ensureBackend(t, "default")
	newDeployment(t, ns, "api", 2)
	prom.setSeries(constantSeries(100))

	ps := baseScaler(ns, "api", v1alpha1.ScaleTargetRef{
		APIVersion: "apps/v1", Kind: "Deployment", Name: "api",
	})
	ps.Spec.Signals = []v1alpha1.NamedSignal{{
		Name:       "extra",
		Prometheus: &v1alpha1.PrometheusSignal{Address: prom.URL, Query: "sum(x)"},
		Capacity:   v1alpha1.CapacitySpec{PerReplica: quantityPtr("10")},
	}}
	if err := k8sClient.Create(context.Background(), ps); err != nil {
		t.Fatalf("create PredictiveScaler: %v", err)
	}

	eventually(t, 30*time.Second, func() error {
		got := getScaler(t, ns, "api")
		status, reason := conditionStatus(got, v1alpha1.ConditionReady)
		if status != metav1.ConditionFalse || reason != "EvaluationFailed" {
			return fmt.Errorf("Ready = %q/%q, want False/EvaluationFailed", status, reason)
		}
		return nil
	})
}

func quantityPtr(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}
