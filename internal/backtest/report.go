package backtest

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// Report renders a comparison of scored strategies.
//
// The numbers that matter are relative. "presage averaged 11.4 replicas" means
// nothing on its own; "presage averaged 8% fewer replicas than the reactive
// policy while being short on a third as many steps" is a decision. So the
// report is anchored on two reference strategies: the reactive baseline, which
// is what you are replacing, and the oracle, which is the best any forecaster
// could do and therefore sets the scale of the prize.
func Report(scores []Score, opts Options, isoCost map[string]IsoCost) string {
	var b strings.Builder

	baseline := find(scores, func(s Score) bool { return strings.HasPrefix(s.Strategy, "reactive") })
	oracle := find(scores, func(s Score) bool { return strings.HasPrefix(s.Strategy, "oracle") })

	fmt.Fprintf(&b, "# Backtest\n\n")
	fmt.Fprintf(&b, "- steps scored: **%d** at %s resolution (%s of history)\n",
		scores[0].Steps, opts.Resolution, (time.Duration(scores[0].Steps) * opts.Resolution).Round(time.Hour))
	fmt.Fprintf(&b, "- lead time: **%s** (%d steps)\n", opts.LeadTime,
		int(math.Ceil(float64(opts.LeadTime)/float64(opts.Resolution))))
	fmt.Fprintf(&b, "- capacity: **%g** signal units per replica\n", opts.PerReplica)
	fmt.Fprintf(&b, "- decisions every **%s**\n\n", time.Duration(opts.EvalEvery)*opts.Resolution)

	fmt.Fprintf(&b, "| strategy | avg replicas | cost vs reactive | short on | unmet (total) | p95 unmet | max unmet | scale ops | errors |\n")
	fmt.Fprintf(&b, "| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |\n")

	for _, s := range scores {
		cost := "—"
		if baseline != nil && baseline.AvgReplicas() > 0 && s.Strategy != baseline.Strategy {
			cost = fmt.Sprintf("%+.1f%%", (s.AvgReplicas()/baseline.AvgReplicas()-1)*100)
		} else if s.Strategy == baseline.Strategy {
			cost = "baseline"
		}
		fmt.Fprintf(&b, "| %s | %.2f | %s | %.1f%% of steps | %.0f | %.1f | %.1f | %d | %d |\n",
			s.Strategy, s.AvgReplicas(), cost, s.UnmetStepFraction()*100,
			s.UnmetDemand, s.P95Unmet, s.MaxUnmet, s.ScaleOps, s.Errors)
	}

	b.WriteString("\n")

	if baseline == nil {
		return b.String()
	}

	b.WriteString("## Read this way\n\n")
	b.WriteString("**Cost** is average provisioned replicas. **Short on** is the share of steps where\n")
	b.WriteString("demand exceeded provisioned capacity — the service-quality cost of being late.\n")
	b.WriteString("A strategy is only better if it improves one without giving back the other.\n\n")

	for _, s := range scores {
		if s.Strategy == baseline.Strategy || strings.HasPrefix(s.Strategy, "oracle") ||
			strings.HasPrefix(s.Strategy, "static") {
			continue
		}
		fmt.Fprintf(&b, "### %s\n\n", s.Strategy)

		costDelta := (s.AvgReplicas()/baseline.AvgReplicas() - 1) * 100
		unmetDelta := s.UnmetDemand - baseline.UnmetDemand

		switch {
		case unmetDelta <= 0 && costDelta <= 0:
			fmt.Fprintf(&b, "Better on both axes: %.1f%% fewer replicas and %.0f less unmet demand.\n",
				-costDelta, -unmetDelta)
		case unmetDelta > 0 && costDelta > 0:
			fmt.Fprintf(&b, "Worse on both axes: %.1f%% more replicas *and* %.0f more unmet demand. "+
				"This configuration is not worth running.\n", costDelta, unmetDelta)
		case unmetDelta < 0:
			fmt.Fprintf(&b, "Bought %.0f less unmet demand for %.1f%% more replicas. "+
				"Whether that trade is worth it is a product decision, not a technical one.\n",
				-unmetDelta, costDelta)
		default:
			fmt.Fprintf(&b, "Saved %.1f%% of replicas at the price of %.0f more unmet demand.\n",
				-costDelta, unmetDelta)
		}

		/* The comparison that actually decides anything. Reducing unmet demand
		 * by spending more is not a result -- a reactive policy with a bigger
		 * buffer would do the same for the same money. So the headline is:
		 * at the same average replica count, who is short less often? */
		iso, ok := isoCost[s.Strategy]
		if !ok {
			b.WriteString("\n")
			continue
		}

		b.WriteString("\n**At equal cost.** ")
		if !iso.Matched {
			fmt.Fprintf(&b, "No reactive buffer reproduces this strategy's average of %.2f\n"+
				"replicas (closest was %s at %.2f), so there is no fair comparison to draw here.\n"+
				"That usually means maxReplicas is binding.\n",
				s.AvgReplicas(), iso.Buffer, iso.Score.AvgReplicas())
			b.WriteString("\n")
			continue
		}

		fmt.Fprintf(&b, "A reactive policy tuned to the same spend (%s buffer, %.2f replicas)\n"+
			"would have been short on **%.1f%%** of steps against this strategy's **%.1f%%**",
			iso.Buffer, iso.Score.AvgReplicas(),
			iso.Score.UnmetStepFraction()*100, s.UnmetStepFraction()*100)

		switch {
		case iso.Score.UnmetDemand > s.UnmetDemand*1.05:
			fmt.Fprintf(&b, ", and carried %.0f more unmet demand overall.\nForecasting is earning its keep here.\n",
				iso.Score.UnmetDemand-s.UnmetDemand)
		case s.UnmetDemand > iso.Score.UnmetDemand*1.05:
			fmt.Fprintf(&b, ", and carried %.0f *less* unmet demand overall.\n"+
				"Spending the same money on a bigger reactive buffer would beat this\nconfiguration. Do that instead.\n",
				s.UnmetDemand-iso.Score.UnmetDemand)
		default:
			b.WriteString(", within noise of each other.\nForecasting is not buying anything on this workload; the cheaper option wins.\n")
		}

		if oracle != nil {
			gap := iso.Score.UnmetDemand - oracle.UnmetDemand
			if gap > 1e-9 {
				captured := (iso.Score.UnmetDemand - s.UnmetDemand) / gap * 100
				fmt.Fprintf(&b, "\nThat closes **%.0f%%** of the gap between an equally expensive reactive\n"+
					"policy and perfect foresight. Near zero means forecasting adds nothing here;\n"+
					"near 100 means the signal is close to perfectly predictable.\n", captured)
			}
		}
		b.WriteString("\n")
	}

	return b.String()
}

func find(scores []Score, pred func(Score) bool) *Score {
	for i := range scores {
		if pred(scores[i]) {
			return &scores[i]
		}
	}
	return nil
}
