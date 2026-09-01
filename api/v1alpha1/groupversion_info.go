// Package v1alpha1 contains the presage autoscaling API.
//
// +kubebuilder:object:generate=true
// +groupName=scaling.presage.sh
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the group/version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "scaling.presage.sh", Version: "v1alpha1"}

	// SchemeBuilder registers the Go types with a scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group/version to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
