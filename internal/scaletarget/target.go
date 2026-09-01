// Package scaletarget applies a replica recommendation to a workload.
package scaletarget

import (
	"context"
	"fmt"
	"time"

	autoscalingv1 "k8s.io/api/autoscaling/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/GrowlyX/presage/api/v1alpha1"
	"github.com/GrowlyX/presage/internal/agones"
)

// Target reads and writes a workload's replica count.
type Target interface {
	// Current reads the target's present replica count.
	Current(ctx context.Context) (int32, error)
	// Apply sets the replica count. Implementations that do not write
	// directly -- the Agones adapter, for instance -- publish the value for
	// whoever does.
	Apply(ctx context.Context, replicas int32) error
	// Describe names the target for logs and events.
	Describe() string
}

// AgonesFleetGVK is the Agones Fleet type. It is referenced by GVK rather than
// by importing the Agones API, so presage has no build or runtime dependency
// on Agones and installs cleanly on clusters that do not run it.
var AgonesFleetGVK = schema.GroupVersionKind{
	Group:   "agones.dev",
	Version: "v1",
	Kind:    "Fleet",
}

// IsAgonesFleet reports whether a target reference points at an Agones Fleet.
func IsAgonesFleet(ref v1alpha1.ScaleTargetRef) bool {
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		return false
	}
	return gv.Group == AgonesFleetGVK.Group && ref.Kind == AgonesFleetGVK.Kind
}

// ScaleSubresource drives anything exposing the standard `scale` subresource:
// Deployments, StatefulSets, ReplicaSets, and custom resources that implement
// it. Going through /scale rather than patching a `spec.replicas` field keeps
// presage working against resources whose replica field lives elsewhere, and
// respects whatever validation the owning controller puts on scaling.
type ScaleSubresource struct {
	Client client.Client
	Ref    v1alpha1.ScaleTargetRef
	Key    types.NamespacedName
}

func (s *ScaleSubresource) object() (*unstructured.Unstructured, error) {
	gv, err := schema.ParseGroupVersion(s.Ref.APIVersion)
	if err != nil {
		return nil, fmt.Errorf("scaletarget: bad apiVersion %q: %w", s.Ref.APIVersion, err)
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gv.WithKind(s.Ref.Kind))
	obj.SetNamespace(s.Key.Namespace)
	obj.SetName(s.Key.Name)
	return obj, nil
}

// scaleGVK is autoscaling/v1 Scale, the type every `scale` subresource speaks.
var scaleGVK = autoscalingv1.SchemeGroupVersion.WithKind("Scale")

// getScale reads the target's scale subresource.
//
// Both the primary object and the subresource body are unstructured, and that
// pairing is load-bearing: controller-runtime picks its client from the
// *primary* object's type, so an unstructured primary routes to the
// unstructured client, which then rejects a typed *autoscalingv1.Scale body
// with "unstructured client did not understand object". Keeping the primary
// unstructured is what makes this work against arbitrary CRDs, so the body
// has to be unstructured too.
func (s *ScaleSubresource) getScale(ctx context.Context) (*unstructured.Unstructured, *unstructured.Unstructured, int32, error) {
	obj, err := s.object()
	if err != nil {
		return nil, nil, 0, err
	}
	scale := &unstructured.Unstructured{}
	scale.SetGroupVersionKind(scaleGVK)

	if err := s.Client.SubResource("scale").Get(ctx, obj, scale); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil, 0, fmt.Errorf("scaletarget: %s not found", s.Describe())
		}
		return nil, nil, 0, fmt.Errorf("scaletarget: read scale of %s: %w", s.Describe(), err)
	}

	replicas, found, err := unstructured.NestedInt64(scale.Object, "spec", "replicas")
	if err != nil {
		return nil, nil, 0, fmt.Errorf("scaletarget: read spec.replicas of %s: %w", s.Describe(), err)
	}
	if !found {
		// A scale subresource without spec.replicas is not something to guess
		// at: treating it as zero could scale a workload to nothing.
		return nil, nil, 0, fmt.Errorf(
			"scaletarget: scale subresource of %s has no spec.replicas", s.Describe())
	}
	return obj, scale, int32(replicas), nil //nolint:gosec // replica counts are small
}

// Current implements Target.
func (s *ScaleSubresource) Current(ctx context.Context) (int32, error) {
	_, _, replicas, err := s.getScale(ctx)
	return replicas, err
}

// Apply implements Target.
func (s *ScaleSubresource) Apply(ctx context.Context, replicas int32) error {
	obj, scale, current, err := s.getScale(ctx)
	if err != nil {
		return err
	}
	if current == replicas {
		return nil
	}
	if err := unstructured.SetNestedField(scale.Object, int64(replicas), "spec", "replicas"); err != nil {
		return fmt.Errorf("scaletarget: set spec.replicas for %s: %w", s.Describe(), err)
	}
	if err := s.Client.SubResource("scale").Update(ctx, obj,
		client.WithSubResourceBody(scale)); err != nil {
		return fmt.Errorf("scaletarget: scale %s to %d: %w", s.Describe(), replicas, err)
	}
	return nil
}

// Describe implements Target.
func (s *ScaleSubresource) Describe() string {
	return fmt.Sprintf("%s %s/%s", s.Ref.Kind, s.Key.Namespace, s.Key.Name)
}

// AgonesFleet publishes recommendations for the FleetAutoscaler webhook rather
// than writing replicas.
//
// Agones must remain the only writer of Fleet replicas. If presage also wrote
// them, the two controllers would fight every sync period, and the Chain
// fallback -- the thing that makes a presage outage harmless -- would be
// bypassed entirely.
type AgonesFleet struct {
	Client client.Client
	Store  *agones.Store
	Key    types.NamespacedName
	// MaxAge bounds how stale a published recommendation may be.
	MaxAge time.Duration
	// Shadow marks published recommendations as advisory only.
	Shadow bool
}

// Current implements Target by reading the Fleet's observed replica count.
func (a *AgonesFleet) Current(ctx context.Context) (int32, error) {
	fleet := &unstructured.Unstructured{}
	fleet.SetGroupVersionKind(AgonesFleetGVK)
	if err := a.Client.Get(ctx, a.Key, fleet); err != nil {
		if apierrors.IsNotFound(err) {
			return 0, fmt.Errorf("scaletarget: %s not found", a.Describe())
		}
		return 0, fmt.Errorf("scaletarget: read %s: %w", a.Describe(), err)
	}
	replicas, found, err := unstructured.NestedInt64(fleet.Object, "status", "replicas")
	if err != nil {
		return 0, fmt.Errorf("scaletarget: read %s status.replicas: %w", a.Describe(), err)
	}
	if !found {
		// A Fleet that has not reported status yet is not an error; it is a
		// Fleet that has just been created.
		return 0, nil
	}
	return int32(replicas), nil //nolint:gosec // replica counts are small
}

// Apply implements Target by caching the recommendation for the webhook.
func (a *AgonesFleet) Apply(_ context.Context, replicas int32) error {
	a.Store.Set(a.Key.Namespace, a.Key.Name, agones.Recommendation{
		Replicas: replicas,
		At:       time.Now(),
		MaxAge:   a.MaxAge,
		Shadow:   a.Shadow,
	})
	return nil
}

// Describe implements Target.
func (a *AgonesFleet) Describe() string {
	return fmt.Sprintf("Fleet %s/%s", a.Key.Namespace, a.Key.Name)
}
