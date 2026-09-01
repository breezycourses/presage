// Package backtest replays a historical signal through scaling strategies and
// scores what each would have done.
//
// This exists because the alternative way to evaluate presage is to run Shadow
// mode forward in real time, which takes weeks and only ever produces one
// sample of one configuration. A backtest turns "is this better than what we
// do today" into a question you can answer over lunch, and -- more usefully --
// it can answer it for the seasonal-naive baseline too, which is the number a
// foundation model actually has to beat.
//
// The simulation's one non-obvious property is that it models provisioning
// lead time. Without that, a reactive strategy looks flawless: it observes
// demand and is credited with serving it in the same instant. Lead time is the
// entire reason forecasting is worth anything, so a backtest that omits it
// would report that presage is pointless.
package backtest

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"
)

// Strategy decides a replica count from the history available at a point in
// time. Implementations must not look at future values -- see the guard in
// Run, which enforces it by only ever passing a prefix.
type Strategy interface {
	Name() string
	// Decide returns the target replica count given the series up to and
	// including index `now`, and the count currently running.
	Decide(ctx context.Context, history []float64, now int, current int32) (int32, error)
}

// Options configure a simulation.
type Options struct {
	// Series is the full observed signal, oldest first.
	Series []float64
	// Resolution is the spacing between points.
	Resolution time.Duration
	// PerReplica is how many units of the signal one replica serves.
	PerReplica float64
	// LeadTime is how long a scale-up takes to become useful. Scale-downs are
	// modelled as immediate: releasing a replica is fast, and pretending
	// otherwise would flatter every strategy equally but unrealistically.
	LeadTime time.Duration
	// Warmup is how many leading points to reserve as history that strategies
	// may read but are not scored on. Must be enough for the backend in use --
	// a seasonal-naive backend needs at least one full season.
	Warmup int
	// EvalEvery is how many steps between decisions, matching a real
	// reconcile interval. One means "decide at every point".
	EvalEvery int
	// InitialReplicas is where every strategy starts, so none is advantaged by
	// its starting position.
	InitialReplicas int32
}

// Score is what one strategy would have cost and cost you.
type Score struct {
	Strategy string

	// Steps scored (excludes warmup).
	Steps int

	// ReplicaSteps is the sum of running replicas over all scored steps: the
	// cost proxy. Divide by Steps for the average replica count.
	ReplicaSteps float64

	// UnmetDemand is the total signal units that exceeded provisioned
	// capacity, summed over steps. This is the service-quality proxy.
	UnmetDemand float64
	// UnmetSteps is how many steps had any unmet demand at all.
	UnmetSteps int
	// MaxUnmet and P95Unmet describe how bad the bad steps were. A strategy
	// that is short by a rounding error for many steps is very different from
	// one that is short by half the fleet for a few.
	MaxUnmet float64
	P95Unmet float64

	// ScaleOps is how many times the replica count changed. Churn has real
	// costs -- connection draining, cold caches, scheduler pressure -- that
	// neither of the headline numbers captures.
	ScaleOps int

	// Errors is how many decisions failed and were held at the previous count.
	Errors int

	// Trace is the per-step history, kept so the run can be plotted rather
	// than only summarised. A table of averages hides the thing you most want
	// to see -- *when* a strategy was short, and whether it was short during
	// the ramps or scattered through the noise.
	Trace Trace
}

// Trace is the per-step record of one strategy's run. All slices are the same
// length and are indexed from the first scored step.
type Trace struct {
	// Provisioned is the replica count actually running at each step.
	Provisioned []int32
	// Required is the replica count demand needed at each step.
	Required []int32
	// Unmet is the shortfall in signal units at each step.
	Unmet []float64
}

// AvgReplicas is the mean provisioned replica count over the scored window.
func (s Score) AvgReplicas() float64 {
	if s.Steps == 0 {
		return 0
	}
	return s.ReplicaSteps / float64(s.Steps)
}

// UnmetStepFraction is the share of steps that were under-provisioned.
func (s Score) UnmetStepFraction() float64 {
	if s.Steps == 0 {
		return 0
	}
	return float64(s.UnmetSteps) / float64(s.Steps)
}

// Run replays the series through one strategy.
//
// The strategy only ever receives `series[:now+1]`, so a lookahead bug in a
// strategy is impossible rather than merely discouraged.
func Run(ctx context.Context, opts Options, strategy Strategy) (Score, error) {
	if err := opts.validate(); err != nil {
		return Score{}, err
	}

	leadSteps := int(math.Ceil(float64(opts.LeadTime) / float64(opts.Resolution)))
	if leadSteps < 0 {
		leadSteps = 0
	}

	steps := len(opts.Series) - opts.Warmup
	score := Score{
		Strategy: strategy.Name(),
		Trace: Trace{
			Provisioned: make([]int32, 0, steps),
			Required:    make([]int32, 0, steps),
			Unmet:       make([]float64, 0, steps),
		},
	}
	current := opts.InitialReplicas

	/* Replicas in flight are modelled as arriving cohorts rather than as a
	 * single pending target, because a cohort is what actually happens: when
	 * the desired count rises from A to B, the B-A new pods start booting and
	 * pods already booting keep their own ETAs. They do not restart.
	 *
	 * Modelling it as one pending target instead means re-asserting the same
	 * desired count every reconcile pushes the arrival time back a step each
	 * time, so on any continuously rising signal the scale-up never lands at
	 * all -- which makes every strategy look equally bad and the whole
	 * backtest worthless. */
	type arrival struct {
		at    int
		count int32
	}
	var inFlight []arrival
	unmets := make([]float64, 0, len(opts.Series))

	for now := opts.Warmup; now < len(opts.Series); now++ {
		// Land any cohort that has finished booting.
		remaining := inFlight[:0]
		for _, a := range inFlight {
			if a.at <= now {
				current += a.count
			} else {
				remaining = append(remaining, a)
			}
		}
		inFlight = remaining

		// Score this step against what is actually running.
		demand := opts.Series[now]
		capacity := float64(current) * opts.PerReplica
		unmet := math.Max(0, demand-capacity)

		score.Steps++
		score.ReplicaSteps += float64(current)
		score.UnmetDemand += unmet
		if unmet > 0 {
			score.UnmetSteps++
			score.MaxUnmet = math.Max(score.MaxUnmet, unmet)
		}
		unmets = append(unmets, unmet)

		score.Trace.Provisioned = append(score.Trace.Provisioned, current)
		score.Trace.Required = append(score.Trace.Required,
			int32(math.Ceil(demand/opts.PerReplica))) //nolint:gosec // replica counts are small
		score.Trace.Unmet = append(score.Trace.Unmet, unmet)

		// Decide, on the configured cadence.
		if (now-opts.Warmup)%opts.EvalEvery != 0 {
			continue
		}
		target, err := strategy.Decide(ctx, opts.Series[:now+1], now, current)
		if err != nil {
			// A failed decision holds the previous count, which is what the
			// controller does: it would rather leave a workload alone than
			// resize it from bad data.
			score.Errors++
			continue
		}

		// committed is what the workload is heading toward: running plus
		// still-booting. Comparing the target against this rather than against
		// `current` is what stops a re-asserted target from being counted, and
		// rescheduled, as a fresh scaling operation.
		committed := current
		for _, a := range inFlight {
			committed += a.count
		}
		if target == committed {
			continue
		}
		score.ScaleOps++

		if target > committed {
			inFlight = append(inFlight, arrival{at: now + leadSteps, count: target - committed})
			continue
		}

		// Scaling down: cancel the newest in-flight cohorts first, then take
		// the remainder from what is already running. Removing a replica is
		// immediate.
		excess := committed - target
		for i := len(inFlight) - 1; i >= 0 && excess > 0; i-- {
			if inFlight[i].count <= excess {
				excess -= inFlight[i].count
				inFlight = inFlight[:i]
			} else {
				inFlight[i].count -= excess
				excess = 0
			}
		}
		if excess > 0 {
			current -= excess
		}
	}

	score.P95Unmet = quantile(unmets, 0.95)
	return score, nil
}

// RunAll replays the same series through every strategy, so the comparison is
// like-for-like by construction.
func RunAll(ctx context.Context, opts Options, strategies ...Strategy) ([]Score, error) {
	scores := make([]Score, 0, len(strategies))
	for _, s := range strategies {
		score, err := Run(ctx, opts, s)
		if err != nil {
			return nil, fmt.Errorf("backtest %s: %w", s.Name(), err)
		}
		scores = append(scores, score)
	}
	return scores, nil
}

func (o Options) validate() error {
	switch {
	case len(o.Series) == 0:
		return fmt.Errorf("backtest: series is empty")
	case o.Resolution <= 0:
		return fmt.Errorf("backtest: resolution must be > 0")
	case o.PerReplica <= 0:
		return fmt.Errorf("backtest: perReplica must be > 0")
	case o.Warmup < 0 || o.Warmup >= len(o.Series):
		return fmt.Errorf("backtest: warmup %d does not leave any steps to score in a series of %d",
			o.Warmup, len(o.Series))
	case o.EvalEvery < 1:
		return fmt.Errorf("backtest: evalEvery must be >= 1")
	}
	return nil
}

func quantile(xs []float64, q float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sorted := make([]float64, len(xs))
	copy(sorted, xs)
	sort.Float64s(sorted)
	pos := q * float64(len(sorted)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return sorted[lo]
	}
	return sorted[lo] + (sorted[hi]-sorted[lo])*(pos-float64(lo))
}
