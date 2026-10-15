package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/breezycourses/presage/api/v1alpha1"
	"github.com/breezycourses/presage/internal/obs"
)

func TestUpdateStatusCountsErrors(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	gr := schema.GroupResource{Group: "scaling.presage.sh", Resource: "predictivescalers"}

	cases := []struct {
		name    string
		err     error
		wantInc float64
	}{
		{"success", nil, 0},
		{"conflict", apierrors.NewConflict(gr, "ps", errors.New("stale")), 0},
		{"rejected", apierrors.NewForbidden(gr, "ps", errors.New("denied")), 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ps := &v1alpha1.PredictiveScaler{ObjectMeta: metav1.ObjectMeta{Namespace: "ns-" + tc.name, Name: "ps"}}
			c := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(ps).
				WithStatusSubresource(ps).
				WithInterceptorFuncs(interceptor.Funcs{
					SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
						if tc.err != nil {
							return tc.err
						}
						return c.SubResource(sub).Update(ctx, obj, opts...)
					},
				}).
				Build()
			r := &PredictiveScalerReconciler{Client: c, Scheme: scheme}

			counter := obs.StatusUpdateErrors.WithLabelValues(ps.Namespace, ps.Name)
			before := testutil.ToFloat64(counter)
			if err := r.updateStatus(context.Background(), ps); !errors.Is(err, tc.err) {
				t.Fatalf("updateStatus error = %v, want %v", err, tc.err)
			}
			if got := testutil.ToFloat64(counter) - before; got != tc.wantInc {
				t.Errorf("counter increased by %v, want %v", got, tc.wantInc)
			}
		})
	}
}
