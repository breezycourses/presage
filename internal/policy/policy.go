// Package policy turns a predictive distribution into a replica count.
//
// It is deliberately free of Kubernetes and forecasting dependencies: the
// caller resolves everything to plain numbers first. That keeps the part of
// presage most likely to be wrong -- and most in need of scrutiny -- a pure
// function that can be exhaustively tested and reasoned about.
package policy

import (
	"errors"
	"fmt"
	"math"
	"time"
)

// Amount is either an absolute quantity or a percentage of some base.
type Amount struct {
	Value   float64
	Percent bool
}

// Of resolves the amount against a base value.
func (a Amount) Of(base float64) float64 {
	if a.Percent {
		return base * a.Value / 100
	}
	return a.Value
}

// String renders the amount the way it was configured.
func (a Amount) String() string {
	if a.Percent {
		return fmt.Sprintf("%g%%", a.Value)
	}
	return fmt.Sprintf("%g", a.Value)
}

// ReactiveFloor mirrors a conventional buffer autoscaler.
type ReactiveFloor struct {
	// Buffer is the spare capacity to keep beyond current demand, as replicas
	// or as a percentage of the replicas current demand already needs.
	Buffer Amount
}

// Config is the resolved policy configuration.
type Config struct {
	// Headroom is added on top of the forecast before converting to replicas.
	Headroom Amount

	MinReplicas int32
	MaxReplicas int32

	// ReactiveFloor, when non-nil, lower-bounds the recommendation by what a
	// reactive autoscaler would have chosen from present demand. With it set,
	// a wrong forecast can only over-provision, never under-provision.
	//
	// Caveat, by design: MaxReplicas and MaxScaleUpRate are hard constraints
	// and can still bind below the floor. The floor removes forecast error as
	// a cause of under-provisioning; it does not override explicit limits.
	ReactiveFloor *ReactiveFloor

	// ScaleDownMaxRelativeSpread refuses a scale-down while the forecast is
	// too uncertain, measured as (upper - lower) / max(lower, 1). Zero
	// disables the guard.
	//
	// This is the second quantile's job. Releasing capacity is the expensive
	// direction to be wrong in, so it is gated on the model being confident;
	// adding capacity is not gated at all, because the upper quantile already
	// grows with uncertainty and provisions for it.
	ScaleDownMaxRelativeSpread float64

	// ScaleDownWindow is how long the recommendation must remain below the
	// current replica count before a decrease is permitted.
	ScaleDownWindow time.Duration

	// MaxScaleUpRate and MaxScaleDownRate cap a single step, relative to the
	// current replica count. A resolved step below one replica is rounded up
	// to one, so a percentage rate can never deadlock at low replica counts.
	MaxScaleUpRate   Amount
	MaxScaleDownRate Amount
}

// Signal is one demand dimension: a forecast, present demand, and the capacity
// one replica provides for it.
type Signal struct {
	// Name identifies the signal in the decision trace.
	Name string

	// PerReplica is how many units of this signal one replica serves.
	PerReplica float64

	// ForecastUp is the forecast at the target quantile, evaluated at the lead
	// time. It alone sets the capacity target: a more uncertain forecast has a
	// higher upper quantile and therefore provisions more, which is the
	// behaviour you want from uncertainty.
	//
	// ForecastDown is the forecast at the lower quantile. It never sets the
	// target; it is only used to measure how confident the forecast is, for
	// the scale-down uncertainty guard.
	ForecastUp   float64
	ForecastDown float64

	// CurrentDemand is the most recent observed value. Only used by the
	// reactive floor.
	CurrentDemand float64
}

// Input is the per-evaluation state.
type Input struct {
	Now time.Time

	// CurrentReplicas as last read from the target.
	CurrentReplicas int32

	// Signals are the demand dimensions to satisfy. Each is converted to a
	// replica requirement independently and the largest wins, the way an HPA
	// combines multiple metrics: a workload has to be big enough for every
	// dimension, so the binding one is whichever needs the most.
	//
	// Averaging or summing instead would let a quiet dimension mask a busy one,
	// which is the failure this design exists to avoid.
	Signals []Signal

	// ScaleDownCandidateSince is when the recommendation first dropped below
	// the current replica count, carried across evaluations in status.
	ScaleDownCandidateSince *time.Time
}

// Constraint names the rule that determined the final replica count.
type Constraint string

const (
	ConstraintNone              Constraint = ""
	ConstraintReactiveFloor     Constraint = "ReactiveFloor"
	ConstraintForecastUncertain Constraint = "ForecastUncertainty"
	ConstraintScaleDownWindow   Constraint = "ScaleDownWindow"
	ConstraintMaxScaleUpRate    Constraint = "MaxScaleUpRate"
	ConstraintMaxScaleDownRate  Constraint = "MaxScaleDownRate"
	ConstraintMinReplicas       Constraint = "MinReplicas"
	ConstraintMaxReplicas       Constraint = "MaxReplicas"
)

// Decision is the outcome of one evaluation.
type Decision struct {
	// Replicas is the recommendation.
	Replicas int32

	// Predictive is what the forecast alone implied, before any floor,
	// stabilization, rate limit, or clamp.
	Predictive int32

	// Reactive is what the reactive floor implied, if it is enabled.
	Reactive *int32

	// Constraint is the last rule that moved the number away from the
	// predictive value, or ConstraintNone if the forecast bound.
	Constraint Constraint

	// BindingSignal names the signal that required the most replicas. With one
	// signal this is trivially that signal; with several it answers "which
	// dimension is actually driving the size of this workload", which is the
	// first thing anyone asks.
	BindingSignal string

	// ScaleDownCandidateSince is the tracker to persist for the next
	// evaluation. Nil means "not currently a scale-down candidate".
	ScaleDownCandidateSince *time.Time

	// Explain is a short human-readable trace of how Replicas was reached.
	Explain string
}

var (
	// ErrInvalidCapacity is returned when per-replica capacity is not usable.
	ErrInvalidCapacity = errors.New("policy: perReplica capacity must be > 0 and finite")
	// ErrInvalidForecast is returned for NaN or infinite forecast values.
	ErrInvalidForecast = errors.New("policy: forecast values must be finite")
	// ErrInvalidBounds is returned when min/max replicas are inconsistent.
	ErrInvalidBounds = errors.New("policy: maxReplicas must be >= minReplicas and >= 1")
	// ErrNoSignals is returned when there is nothing to scale on.
	ErrNoSignals = errors.New("policy: at least one signal is required")
)

// Evaluate computes a replica recommendation.
//
// The shape of the decision is:
//
//  1. Size capacity to the upper forecast quantile at the lead time. One
//     quantile sets the target in both directions: an uncertain forecast has a
//     fatter upper tail and so provisions more, rather than provisioning the
//     same and hesitating.
//  2. Lower-bound by the reactive floor, so a bad forecast cannot starve the
//     workload.
//  3. Gate scale-downs, and only scale-downs: first on forecast confidence,
//     then on the stabilization window. Scale-ups are never delayed -- being
//     early is the entire point of forecasting.
//  4. Rate-limit the step, then clamp to [MinReplicas, MaxReplicas].
func Evaluate(cfg Config, in Input) (Decision, error) {
	if len(in.Signals) == 0 {
		return Decision{}, ErrNoSignals
	}
	if cfg.MaxReplicas < 1 || cfg.MaxReplicas < cfg.MinReplicas {
		return Decision{}, ErrInvalidBounds
	}

	current := in.CurrentReplicas

	// Step 1: each signal becomes a replica requirement; the largest binds.
	var (
		target     int32
		binding    string
		bindingUp  float64
		bindingLow float64
	)
	for _, sig := range in.Signals {
		if sig.PerReplica <= 0 || math.IsNaN(sig.PerReplica) || math.IsInf(sig.PerReplica, 0) {
			return Decision{}, fmt.Errorf("%w: signal %q", ErrInvalidCapacity, sig.Name)
		}
		if !finite(sig.ForecastUp) || !finite(sig.ForecastDown) || !finite(sig.CurrentDemand) {
			return Decision{}, fmt.Errorf("%w: signal %q", ErrInvalidForecast, sig.Name)
		}

		// Defensive: quantile crossing. TimesFM can be asked to fix this
		// itself, but a backend that does not must never invert the policy.
		up, down := math.Max(sig.ForecastUp, 0), math.Max(sig.ForecastDown, 0)
		if up < down {
			up, down = down, up
		}

		need := replicasFor(cfg, up, sig.PerReplica)
		if binding == "" || need > target {
			target, binding, bindingUp, bindingLow = need, sig.Name, up, down
		}
	}

	predictive := target
	constraint := ConstraintNone
	explain := fmt.Sprintf("signal %q forecast q_target=%.2f -> %d replicas (current %d)",
		binding, bindingUp, target, current)
	up, down := bindingUp, bindingLow

	// Step 2: reactive floor, taken across every signal for the same reason
	// the target is: the workload must be big enough for all of them.
	var reactive *int32
	if cfg.ReactiveFloor != nil {
		var r int32
		var floorSignal string
		var floorDemand float64
		for _, sig := range in.Signals {
			base := int32(math.Ceil(sig.CurrentDemand / sig.PerReplica))
			need := base + int32(math.Ceil(cfg.ReactiveFloor.Buffer.Of(float64(base))))
			if floorSignal == "" || need > r {
				r, floorSignal, floorDemand = need, sig.Name, sig.CurrentDemand
			}
		}
		reactive = &r
		if r > target {
			target = r
			constraint = ConstraintReactiveFloor
			explain += fmt.Sprintf("; reactive floor raised to %d (signal %q demand %.2f + buffer %s)",
				r, floorSignal, floorDemand, cfg.ReactiveFloor.Buffer)
		}
	}

	// Step 3: scale-down gates. Applied after the floor so they act on the
	// number presage would actually apply, not an intermediate one.
	//
	// The candidate tracker keys off `desired` -- the value before these gates
	// -- so that holding at the current count does not reset the window it is
	// waiting on.
	desired := target
	candidateSince := in.ScaleDownCandidateSince
	if desired < current {
		if candidateSince == nil {
			now := in.Now
			candidateSince = &now
		}

		// 3a: forecast confidence.
		if cfg.ScaleDownMaxRelativeSpread > 0 {
			if spread := relativeSpread(up, down); spread > cfg.ScaleDownMaxRelativeSpread {
				target = current
				constraint = ConstraintForecastUncertain
				explain += fmt.Sprintf("; scale-down blocked, forecast spread %.2f > %.2f",
					spread, cfg.ScaleDownMaxRelativeSpread)
			}
		}

		// 3b: stabilization window.
		if constraint != ConstraintForecastUncertain {
			if elapsed := in.Now.Sub(*candidateSince); elapsed < cfg.ScaleDownWindow {
				target = current
				constraint = ConstraintScaleDownWindow
				explain += fmt.Sprintf("; scale-down held (%s of %s elapsed)",
					elapsed.Round(time.Second), cfg.ScaleDownWindow)
			}
		}
	} else {
		candidateSince = nil
	}

	// Step 4a: rate limits.
	switch {
	case target > current:
		step := atLeastOne(cfg.MaxScaleUpRate.Of(float64(current)))
		if limit := current + step; target > limit {
			target = limit
			constraint = ConstraintMaxScaleUpRate
			explain += fmt.Sprintf("; scale-up rate-limited to +%d", step)
		}
	case target < current:
		step := atLeastOne(cfg.MaxScaleDownRate.Of(float64(current)))
		if limit := current - step; target < limit {
			target = limit
			constraint = ConstraintMaxScaleDownRate
			explain += fmt.Sprintf("; scale-down rate-limited to -%d", step)
		}
	}

	// Step 4b: hard bounds, applied last so they always win.
	if target < cfg.MinReplicas {
		target = cfg.MinReplicas
		constraint = ConstraintMinReplicas
		explain += fmt.Sprintf("; clamped up to minReplicas %d", cfg.MinReplicas)
	}
	if target > cfg.MaxReplicas {
		target = cfg.MaxReplicas
		constraint = ConstraintMaxReplicas
		explain += fmt.Sprintf("; clamped down to maxReplicas %d", cfg.MaxReplicas)
	}

	return Decision{
		Replicas:                target,
		Predictive:              predictive,
		Reactive:                reactive,
		Constraint:              constraint,
		BindingSignal:           binding,
		ScaleDownCandidateSince: candidateSince,
		Explain:                 explain,
	}, nil
}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// replicasFor converts a demand figure into replicas, including headroom.
func replicasFor(cfg Config, demand, perReplica float64) int32 {
	withHeadroom := demand + cfg.Headroom.Of(demand)
	return int32(math.Ceil(withHeadroom / perReplica))
}

// relativeSpread measures forecast uncertainty as the gap between the upper
// and lower quantiles, normalised by the lower one. The max(.,1) floor keeps
// the measure finite for near-zero demand, where any absolute gap would
// otherwise read as infinite uncertainty.
func relativeSpread(upper, lower float64) float64 {
	return (upper - lower) / math.Max(lower, 1)
}

// atLeastOne rounds a resolved step up to a whole replica, with a floor of 1
// so that a percentage rate cannot stall at small replica counts.
func atLeastOne(f float64) int32 {
	s := int32(math.Ceil(f))
	if s < 1 {
		return 1
	}
	return s
}
