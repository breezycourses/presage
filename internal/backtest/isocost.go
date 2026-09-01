package backtest

import (
	"context"
	"fmt"
	"math"

	"github.com/GrowlyX/presage/internal/policy"
)

// IsoCost is a reactive baseline tuned to spend the same as another strategy.
type IsoCost struct {
	Score  Score
	Buffer policy.Amount
	// Matched is false when no buffer produced a comparable average replica
	// count, in which case the comparison must not be presented as fair.
	Matched bool
}

// MatchCost finds the reactive buffer whose average replica count is closest
// to target, so two strategies can be compared at equal spend.
//
// This exists because comparing autoscaling strategies on unmet demand alone
// is meaningless: any strategy can reduce unmet demand by provisioning more.
// "presage had 86% less unmet demand" is not a result if presage also ran 10%
// more replicas -- a reactive policy with a bigger buffer would have done the
// same thing for the same money, and possibly better.
//
// The honest question is: at the same spend, who is short less often? That is
// what this makes answerable.
func MatchCost(ctx context.Context, opts Options, target float64, min, max int32) (IsoCost, error) {
	if target <= 0 {
		return IsoCost{}, fmt.Errorf("backtest: iso-cost target must be > 0")
	}

	// The reactive buffer is monotonic in cost, so a bisection is safe.
	lo, hi := 0.0, 500.0
	var best IsoCost
	bestErr := math.Inf(1)

	for i := 0; i < 24; i++ {
		mid := (lo + hi) / 2
		buffer := policy.Amount{Value: mid, Percent: true}
		score, err := Run(ctx, opts, Reactive{
			PerReplica: opts.PerReplica, Buffer: buffer, Min: min, Max: max,
		})
		if err != nil {
			return IsoCost{}, err
		}

		if d := math.Abs(score.AvgReplicas() - target); d < bestErr {
			// Round the label: a bisection produces buffers like
			// "20.5078125%", which is precision the reader cannot use and
			// makes the report look like a machine talking to itself.
			bestErr, best = d, IsoCost{
				Score:  score,
				Buffer: policy.Amount{Value: math.Round(mid*10) / 10, Percent: true},
			}
		}
		if score.AvgReplicas() < target {
			lo = mid
		} else {
			hi = mid
		}
	}

	// Within 2% of the target is close enough to call it the same spend.
	// Beyond that, say so rather than presenting an unfair comparison as a
	// fair one -- usually it means maxReplicas is binding, and no buffer can
	// reach the target.
	best.Matched = bestErr/target <= 0.02
	best.Score.Strategy = fmt.Sprintf("reactive @ iso-cost (buffer=%s)", best.Buffer)
	return best, nil
}
