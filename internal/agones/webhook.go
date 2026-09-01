// Package agones implements the Agones FleetAutoscaler webhook adapter.
//
// presage never writes Fleet replicas itself. Agones stays the single writer;
// presage only answers the question Agones already asks on its own schedule.
// That has a specific operational payoff: paired with a Chain policy, a
// presage outage degrades to a plain Buffer policy instead of freezing or
// mis-sizing the fleet.
//
//	policy:
//	  type: Chain
//	  chain:
//	    - {id: predictive, type: Webhook, webhook: {service: {...}}}
//	    - {id: fallback,   type: Buffer,  buffer: {bufferSize: 2, maxReplicas: 40}}
//
// The webhook deliberately does not forecast inline. Agones polls every 30s by
// default; a 200M-parameter model on that path would make the model server a
// hard dependency of the scaling loop and put its tail latency directly into
// Agones' control loop. Instead the controller refreshes a recommendation on
// its own cadence and this handler serves the cached value.
package agones

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// FleetStatus mirrors the Agones Fleet status carried in a review request.
type FleetStatus struct {
	Replicas          int32 `json:"replicas"`
	ReadyReplicas     int32 `json:"readyReplicas"`
	ReservedReplicas  int32 `json:"reservedReplicas"`
	AllocatedReplicas int32 `json:"allocatedReplicas"`
	Allocations       int64 `json:"allocations"`
}

// FleetAutoscaleRequest is the request half of a FleetAutoscaleReview.
type FleetAutoscaleRequest struct {
	UID         string            `json:"uid"`
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace"`
	Status      FleetStatus       `json:"status"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// FleetAutoscaleResponse is the response half of a FleetAutoscaleReview.
type FleetAutoscaleResponse struct {
	UID      string `json:"uid"`
	Scale    bool   `json:"scale"`
	Replicas int32  `json:"replicas"`
}

// FleetAutoscaleReview is the payload exchanged with Agones.
type FleetAutoscaleReview struct {
	Request  *FleetAutoscaleRequest  `json:"request"`
	Response *FleetAutoscaleResponse `json:"response"`
}

// Recommendation is a cached scaling decision for one fleet.
type Recommendation struct {
	Replicas int32
	// At is when the recommendation was computed, used for staleness checks.
	At time.Time
	// MaxAge is how stale this recommendation may be before the webhook stops
	// answering. Zero means the store default applies.
	MaxAge time.Duration
	// Shadow marks a recommendation that must not be acted on. The handler
	// still answers, but with scale=false, so that Agones records a
	// well-formed review and a Chain policy falls through to its next entry.
	Shadow bool
}

// Store holds the current recommendation per fleet.
type Store struct {
	mu            sync.RWMutex
	items         map[string]Recommendation
	defaultMaxAge time.Duration
}

// NewStore builds an empty store.
func NewStore(defaultMaxAge time.Duration) *Store {
	if defaultMaxAge <= 0 {
		defaultMaxAge = 5 * time.Minute
	}
	return &Store{items: make(map[string]Recommendation), defaultMaxAge: defaultMaxAge}
}

// Key is the store key for a fleet.
func Key(namespace, name string) string { return namespace + "/" + name }

// Set records a recommendation.
func (s *Store) Set(namespace, name string, rec Recommendation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[Key(namespace, name)] = rec
}

// Delete drops a fleet's recommendation, e.g. when its PredictiveScaler goes
// away. Leaving a stale entry behind would let a deleted scaler keep answering
// until the staleness window expired.
func (s *Store) Delete(namespace, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.items, Key(namespace, name))
}

// Get returns a recommendation and whether it exists.
func (s *Store) Get(namespace, name string) (Recommendation, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.items[Key(namespace, name)]
	return rec, ok
}

// Handler serves FleetAutoscaleReview requests from the store.
type Handler struct {
	Store *Store
	// Now is injectable for tests.
	Now func() time.Time
	// OnServed is an optional hook for metrics and logging.
	OnServed func(namespace, name string, served bool, reason string)
}

// ServeHTTP implements http.Handler.
//
// The path carries the fleet identity so a single service can back many
// FleetAutoscalers: /scale/<namespace>/<name>. A bare /scale falls back to the
// namespace and name Agones puts in the request body, which is what the
// stock Agones examples send.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var review FleetAutoscaleReview
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&review); err != nil {
		http.Error(w, fmt.Sprintf("decode review: %v", err), http.StatusBadRequest)
		return
	}
	if review.Request == nil {
		http.Error(w, "review has no request", http.StatusBadRequest)
		return
	}

	namespace, name := review.Request.Namespace, review.Request.Name
	if ns, n, ok := parsePath(r.URL.Path); ok {
		namespace, name = ns, n
	}
	if namespace == "" || name == "" {
		http.Error(w, "review has no fleet identity", http.StatusBadRequest)
		return
	}

	now := time.Now
	if h.Now != nil {
		now = h.Now
	}

	rec, ok := h.Store.Get(namespace, name)
	switch {
	case !ok:
		// No recommendation yet. Erroring rather than answering scale=false is
		// deliberate: an error makes a Chain policy fall through to its
		// fallback, whereas a well-formed "don't scale" would be taken as an
		// authoritative decision and the fallback would never run.
		h.served(namespace, name, false, "no recommendation")
		http.Error(w, fmt.Sprintf("no recommendation for %s/%s", namespace, name),
			http.StatusServiceUnavailable)
		return

	case rec.Shadow:
		h.served(namespace, name, false, "shadow mode")
		http.Error(w, fmt.Sprintf("%s/%s is in Shadow mode", namespace, name),
			http.StatusServiceUnavailable)
		return
	}

	maxAge := rec.MaxAge
	if maxAge <= 0 {
		maxAge = h.Store.defaultMaxAge
	}
	if age := now().Sub(rec.At); age > maxAge {
		h.served(namespace, name, false, "stale")
		http.Error(w, fmt.Sprintf("recommendation for %s/%s is %s old, past the %s limit",
			namespace, name, age.Round(time.Second), maxAge), http.StatusServiceUnavailable)
		return
	}

	review.Response = &FleetAutoscaleResponse{
		UID:      review.Request.UID,
		Scale:    true,
		Replicas: rec.Replicas,
	}
	h.served(namespace, name, true, "")

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(review); err != nil {
		// The response is already partially written; nothing useful is left to
		// do but record it.
		h.served(namespace, name, false, "encode failed")
	}
}

func (h *Handler) served(namespace, name string, ok bool, reason string) {
	if h.OnServed != nil {
		h.OnServed(namespace, name, ok, reason)
	}
}

// parsePath extracts /scale/<namespace>/<name>.
func parsePath(p string) (namespace, name string, ok bool) {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	if len(parts) != 3 || parts[0] != "scale" {
		return "", "", false
	}
	if parts[1] == "" || parts[2] == "" {
		return "", "", false
	}
	return parts[1], parts[2], true
}
