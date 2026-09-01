// Package e2e runs the presage controller against a real Kubernetes API
// server (envtest: apiserver + etcd, no kubelet).
//
// The unit tests cover each component in isolation against fakes they define
// themselves, which means they can all agree with each other and all be wrong.
// This suite exercises the wiring those tests cannot: CRD schemas actually
// accepting the objects the controller writes, status subresources, finalizer
// ordering, the scale subresource, and the Agones webhook serving what the
// reconciler published.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/breezycourses/presage/api/v1alpha1"
	"github.com/breezycourses/presage/internal/agones"
	"github.com/breezycourses/presage/internal/controller"
)

var (
	testEnv     *envtest.Environment
	cfg         *rest.Config
	k8sClient   client.Client
	agonesStore *agones.Store
	prom        *fakePrometheus
	testCtx     context.Context
	stopManager context.CancelFunc
)

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		fmt.Println("skipping e2e: KUBEBUILDER_ASSETS is unset (run `make test-e2e`)")
		os.Exit(0)
	}

	logf.SetLogger(zap.New(zap.UseDevMode(true), zap.WriteTo(os.Stderr)))

	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "config", "crd"),
			filepath.Join("..", "testdata", "crd"),
		},
		ErrorIfCRDPathMissing: true,
	}

	var err error
	if cfg, err = testEnv.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to start envtest: %v\n", err)
		os.Exit(1)
	}

	scheme := runtime.NewScheme()
	must(clientgoscheme.AddToScheme(scheme))
	must(v1alpha1.AddToScheme(scheme))

	if k8sClient, err = client.New(cfg, client.Options{Scheme: scheme}); err != nil {
		fmt.Fprintf(os.Stderr, "failed to build client: %v\n", err)
		os.Exit(1)
	}

	prom = newFakePrometheus()
	defer prom.Close()

	testCtx, stopManager = context.WithCancel(context.Background())

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:         scheme,
		Metrics:        metricsserver.Options{BindAddress: "0"},
		LeaderElection: false,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to build manager: %v\n", err)
		os.Exit(1)
	}

	agonesStore = agones.NewStore(5 * time.Minute)
	if err := (&controller.PredictiveScalerReconciler{
		Client:      mgr.GetClient(),
		Scheme:      mgr.GetScheme(),
		Recorder:    mgr.GetEventRecorderFor("presage"),
		AgonesStore: agonesStore,
		HTTPClient:  &http.Client{Timeout: 10 * time.Second},
	}).SetupWithManager(mgr); err != nil {
		fmt.Fprintf(os.Stderr, "failed to set up controller: %v\n", err)
		os.Exit(1)
	}

	go func() {
		if err := mgr.Start(testCtx); err != nil {
			fmt.Fprintf(os.Stderr, "manager exited: %v\n", err)
		}
	}()

	code := m.Run()

	stopManager()
	if err := testEnv.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to stop envtest: %v\n", err)
	}
	os.Exit(code)
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

// ---------------------------------------------------------------------------
// fake Prometheus
// ---------------------------------------------------------------------------

// seriesFunc produces the signal value at a given step index, counted from the
// oldest point in the requested range.
type seriesFunc func(step int, total int) float64

// fakePrometheus serves range and instant queries. It generates samples on
// exactly the grid the client asks for, so the client's own resampling and
// gap-filling logic is exercised rather than bypassed.
type fakePrometheus struct {
	*httptest.Server

	mu     sync.Mutex
	series seriesFunc
	scalar float64
	// dropEvery, when > 0, keeps only every Nth sample so gap-filling and the
	// gap-ratio guard are exercised.
	dropEvery int
}

func newFakePrometheus() *fakePrometheus {
	f := &fakePrometheus{
		series: constantSeries(100),
		scalar: 120,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/query_range", f.handleRange)
	mux.HandleFunc("/api/v1/query", f.handleInstant)
	f.Server = httptest.NewServer(mux)
	return f
}

func (f *fakePrometheus) setSeries(fn seriesFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.series = fn
}

func (f *fakePrometheus) setScalar(v float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scalar = v
}

func (f *fakePrometheus) setDropEvery(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dropEvery = n
}

func (f *fakePrometheus) snapshot() (seriesFunc, float64, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.series, f.scalar, f.dropEvery
}

func (f *fakePrometheus) handleRange(w http.ResponseWriter, r *http.Request) {
	form, err := parseForm(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	start, _ := strconv.ParseFloat(form.Get("start"), 64)
	end, _ := strconv.ParseFloat(form.Get("end"), 64)
	step, _ := strconv.ParseFloat(form.Get("step"), 64)
	if step <= 0 {
		http.Error(w, "bad step", http.StatusBadRequest)
		return
	}

	fn, _, dropEvery := f.snapshot()
	total := int((end-start)/step) + 1

	values := make([]json.RawMessage, 0, total)
	for i := 0; i < total; i++ {
		if dropEvery > 0 && i > 0 && i%dropEvery != 0 {
			continue
		}
		ts := start + float64(i)*step
		v := fn(i, total)
		values = append(values, json.RawMessage(
			fmt.Sprintf(`[%s,"%s"]`,
				strconv.FormatFloat(ts, 'f', -1, 64),
				strconv.FormatFloat(v, 'f', 6, 64))))
	}

	writeJSON(w, fmt.Sprintf(
		`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"job":"test"},"values":[%s]}]}}`,
		joinRaw(values)))
}

func (f *fakePrometheus) handleInstant(w http.ResponseWriter, r *http.Request) {
	if _, err := parseForm(r); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_, scalar, _ := f.snapshot()
	writeJSON(w, fmt.Sprintf(
		`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[%d,"%s"]}]}}`,
		time.Now().Unix(), strconv.FormatFloat(scalar, 'f', 6, 64)))
}

func parseForm(r *http.Request) (url.Values, error) {
	if err := r.ParseForm(); err != nil {
		return nil, err
	}
	return r.Form, nil
}

func writeJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

func joinRaw(msgs []json.RawMessage) string {
	out := ""
	for i, m := range msgs {
		if i > 0 {
			out += ","
		}
		out += string(m)
	}
	return out
}

// ---------------------------------------------------------------------------
// series shapes
// ---------------------------------------------------------------------------

func constantSeries(v float64) seriesFunc {
	return func(int, int) float64 { return v }
}

// seasonalSeries repeats a sine of the given period, in steps.
func seasonalSeries(period int, offset, amplitude float64) seriesFunc {
	return func(step, _ int) float64 {
		return offset + amplitude*math.Sin(2*math.Pi*float64(step%period)/float64(period))
	}
}

// quietPastBusyNow produces a series that was low a season ago and is high
// right now. A seasonal-naive forecast therefore predicts low demand while
// actual demand is high -- the exact shape that must trigger the reactive
// floor.
func quietPastBusyNow(period int, quiet, busy float64) seriesFunc {
	return func(step, total int) float64 {
		if step >= total-period/2 {
			return busy
		}
		return quiet
	}
}

func resourceQuantity(s string) resource.Quantity {
	return resource.MustParse(s)
}
