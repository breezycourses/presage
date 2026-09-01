package agones

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

var now = time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)

func review(namespace, name string) *bytes.Buffer {
	body, _ := json.Marshal(FleetAutoscaleReview{
		Request: &FleetAutoscaleRequest{
			UID:       "uid-1",
			Name:      name,
			Namespace: namespace,
			Status:    FleetStatus{Replicas: 5, ReadyReplicas: 2, AllocatedReplicas: 3},
		},
	})
	return bytes.NewBuffer(body)
}

func handler(store *Store) *Handler {
	return &Handler{Store: store, Now: func() time.Time { return now }}
}

func do(t *testing.T, h *Handler, path string, body *bytes.Buffer) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestServesCachedRecommendation(t *testing.T) {
	store := NewStore(5 * time.Minute)
	store.Set("default", "lobby", Recommendation{Replicas: 9, At: now.Add(-time.Minute)})

	rec := do(t, handler(store), "/scale", review("default", "lobby"))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var out FleetAutoscaleReview
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("undecodable response: %v", err)
	}
	if out.Response == nil {
		t.Fatal("response half was not populated")
	}
	if !out.Response.Scale || out.Response.Replicas != 9 {
		t.Fatalf("unexpected response: %+v", out.Response)
	}
	// Agones correlates request and response by UID; dropping it makes the
	// review unmatchable in Agones' logs.
	if out.Response.UID != "uid-1" {
		t.Fatalf("UID was not echoed: %q", out.Response.UID)
	}
}

func TestPathIdentityOverridesBody(t *testing.T) {
	store := NewStore(5 * time.Minute)
	store.Set("games", "kitpvp", Recommendation{Replicas: 3, At: now})

	rec := do(t, handler(store), "/scale/games/kitpvp", review("default", "lobby"))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestUnknownFleetErrorsSoChainFallsThrough pins the safety contract. A
// well-formed scale=false answer would be taken by Agones as an authoritative
// decision and the Chain fallback would never run; an error is what makes the
// fallback fire.
func TestUnknownFleetErrorsSoChainFallsThrough(t *testing.T) {
	rec := do(t, handler(NewStore(5*time.Minute)), "/scale", review("default", "lobby"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 so Chain falls through, got %d", rec.Code)
	}
	if bytes.Contains(rec.Body.Bytes(), []byte(`"scale"`)) {
		t.Fatalf("must not return a well-formed review: %s", rec.Body.String())
	}
}

func TestStaleRecommendationIsRefused(t *testing.T) {
	store := NewStore(5 * time.Minute)
	store.Set("default", "lobby", Recommendation{Replicas: 9, At: now.Add(-10 * time.Minute)})

	rec := do(t, handler(store), "/scale", review("default", "lobby"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 for a stale recommendation, got %d", rec.Code)
	}
}

func TestPerFleetMaxAgeOverridesDefault(t *testing.T) {
	store := NewStore(time.Hour)
	store.Set("default", "lobby", Recommendation{
		Replicas: 9, At: now.Add(-2 * time.Minute), MaxAge: time.Minute,
	})

	rec := do(t, handler(store), "/scale", review("default", "lobby"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 from the per-fleet limit, got %d", rec.Code)
	}
}

func TestShadowModeDoesNotScale(t *testing.T) {
	store := NewStore(5 * time.Minute)
	store.Set("default", "lobby", Recommendation{Replicas: 9, At: now, Shadow: true})

	rec := do(t, handler(store), "/scale", review("default", "lobby"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("shadow mode must not scale, got %d", rec.Code)
	}
}

func TestDeleteStopsServing(t *testing.T) {
	store := NewStore(5 * time.Minute)
	store.Set("default", "lobby", Recommendation{Replicas: 9, At: now})
	store.Delete("default", "lobby")

	rec := do(t, handler(store), "/scale", review("default", "lobby"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("a deleted scaler must stop answering, got %d", rec.Code)
	}
}

func TestRejectsMalformedRequests(t *testing.T) {
	h := handler(NewStore(5 * time.Minute))

	if rec := do(t, h, "/scale", bytes.NewBufferString("{")); rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for undecodable body, got %d", rec.Code)
	}
	if rec := do(t, h, "/scale", bytes.NewBufferString(`{}`)); rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for a review with no request, got %d", rec.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/scale", nil)
	out := httptest.NewRecorder()
	h.ServeHTTP(out, req)
	if out.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405 for GET, got %d", out.Code)
	}
}

func TestParsePath(t *testing.T) {
	tests := []struct {
		path     string
		ns, name string
		ok       bool
	}{
		{"/scale/default/lobby", "default", "lobby", true},
		{"/scale", "", "", false},
		{"/scale/default", "", "", false},
		{"/other/default/lobby", "", "", false},
		{"/scale/default/lobby/extra", "", "", false},
	}
	for _, tt := range tests {
		ns, name, ok := parsePath(tt.path)
		if ok != tt.ok || ns != tt.ns || name != tt.name {
			t.Errorf("parsePath(%q) = (%q,%q,%v), want (%q,%q,%v)",
				tt.path, ns, name, ok, tt.ns, tt.name, tt.ok)
		}
	}
}

func TestConcurrentAccessIsSafe(t *testing.T) {
	store := NewStore(5 * time.Minute)
	h := handler(store)
	done := make(chan struct{})

	go func() {
		for i := 0; i < 500; i++ {
			store.Set("default", "lobby", Recommendation{Replicas: int32(i), At: now})
		}
		close(done)
	}()
	for i := 0; i < 500; i++ {
		do(t, h, "/scale", review("default", "lobby"))
	}
	<-done
}
