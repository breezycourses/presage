// Command presage-backtest replays a historical signal through several scaling
// strategies and reports what each would have done.
//
// The point is to answer "would running presage have been better than what we
// do today" in minutes rather than by running Shadow mode forward for weeks --
// and, just as importantly, to answer it for the seasonal-naive baseline too,
// which is the number a foundation model has to beat before it earns a node.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/GrowlyX/presage/internal/backtest"
	"github.com/GrowlyX/presage/internal/forecast"
	"github.com/GrowlyX/presage/internal/metrics"
	"github.com/GrowlyX/presage/internal/policy"
)

func main() {
	var (
		address    = flag.String("address", "", "Prometheus-compatible query endpoint (required)")
		query      = flag.String("query", "", "PromQL returning exactly one series (required)")
		window     = flag.Duration("window", 21*24*time.Hour, "How much history to replay")
		resolution = flag.Duration("resolution", 5*time.Minute, "Sample spacing")
		leadTime   = flag.Duration("lead-time", 2*time.Minute, "Provisioning lead time")
		interval   = flag.Duration("interval", time.Minute, "How often a decision is made")
		perReplica = flag.Float64("per-replica", 1, "Signal units one replica serves")
		minRep     = flag.Int("min-replicas", 1, "")
		maxRep     = flag.Int("max-replicas", 100, "")
		buffer     = flag.String("buffer", "10%", "Reactive buffer, as a count or a percentage")
		headroom   = flag.String("headroom", "10%", "Predictive headroom")
		targetQ    = flag.Float64("target-quantile", 0.9, "Quantile capacity is sized to")
		lowerQ     = flag.Float64("lower-quantile", 0.5, "Quantile used for the scale-down guard")
		season     = flag.Duration("season", 168*time.Hour, "Seasonality for the naive baseline")
		timesfm    = flag.String("timesfm", "", "presage-forecaster endpoint; omit to skip TimesFM")
		maxContext = flag.Int("max-context", 4096, "TimesFM compiled context length")
		maxHorizon = flag.Int("max-horizon", 64, "TimesFM compiled horizon")
		staticRep  = flag.Int("static", 0, "Also compare against a fixed replica count")
		token      = flag.String("bearer-token", "", "Bearer token for the metrics endpoint")
		out        = flag.String("out", "", "Write the report here instead of stdout")
	)
	flag.Parse()

	if *address == "" || *query == "" {
		fmt.Fprintln(os.Stderr, "both -address and -query are required")
		flag.Usage()
		os.Exit(2)
	}

	if err := run(runConfig{
		address: *address, query: *query, token: *token,
		window: *window, resolution: *resolution, leadTime: *leadTime, interval: *interval,
		perReplica: *perReplica, minRep: int32(*minRep), maxRep: int32(*maxRep),
		buffer: *buffer, headroom: *headroom,
		targetQ: *targetQ, lowerQ: *lowerQ,
		season:  *season,
		timesfm: *timesfm, maxContext: *maxContext, maxHorizon: *maxHorizon,
		staticRep: int32(*staticRep), out: *out,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

type runConfig struct {
	address, query, token        string
	window, resolution, leadTime time.Duration
	interval                     time.Duration
	perReplica                   float64
	minRep, maxRep               int32
	buffer, headroom             string
	targetQ, lowerQ              float64
	season                       time.Duration
	timesfm                      string
	maxContext, maxHorizon       int
	staticRep                    int32
	out                          string
}

func run(cfg runConfig) error {
	ctx := context.Background()

	client := metrics.NewClient(cfg.address, cfg.token, false, 2*time.Minute)
	end := time.Now().Truncate(cfg.resolution)
	start := end.Add(-cfg.window)

	fmt.Fprintf(os.Stderr, "fetching %s of history at %s resolution...\n", cfg.window, cfg.resolution)
	series, err := client.QueryRange(ctx, cfg.query, start, end, cfg.resolution)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "got %d points (%d gap-filled)\n", len(series.Values), series.Gaps)

	if gapRatio := float64(series.Gaps) / float64(series.Steps); gapRatio > 0.5 {
		return fmt.Errorf("signal is %.0f%% gap-filled; a backtest over mostly "+
			"interpolated data would measure the interpolation, not the workload", gapRatio*100)
	}

	seasonSteps := int(cfg.season / cfg.resolution)
	// Reserve enough history that the naive baseline has whole seasons to
	// repeat, and that TimesFM has meaningful context. Scoring starts after.
	warmup := 2 * seasonSteps
	if warmup >= len(series.Values) {
		return fmt.Errorf("need more than %s of history to score against a %s season; got %s",
			2*cfg.season, cfg.season, time.Duration(len(series.Values))*cfg.resolution)
	}

	bufferAmt, err := parseAmount(cfg.buffer)
	if err != nil {
		return fmt.Errorf("-buffer: %w", err)
	}
	headroomAmt, err := parseAmount(cfg.headroom)
	if err != nil {
		return fmt.Errorf("-headroom: %w", err)
	}

	leadSteps := int(cfg.leadTime / cfg.resolution)
	if leadSteps < 1 {
		leadSteps = 1
	}

	opts := backtest.Options{
		Series:          series.Values,
		Resolution:      cfg.resolution,
		PerReplica:      cfg.perReplica,
		LeadTime:        cfg.leadTime,
		Warmup:          warmup,
		EvalEvery:       max(1, int(cfg.interval/cfg.resolution)),
		InitialReplicas: cfg.minRep,
	}

	policyCfg := policy.Config{
		Headroom:                   headroomAmt,
		MinReplicas:                cfg.minRep,
		MaxReplicas:                cfg.maxRep,
		ReactiveFloor:              &policy.ReactiveFloor{Buffer: bufferAmt},
		ScaleDownWindow:            15 * time.Minute,
		MaxScaleUpRate:             policy.Amount{Value: 100, Percent: true},
		MaxScaleDownRate:           policy.Amount{Value: 20, Percent: true},
		ScaleDownMaxRelativeSpread: 0.25,
	}

	strategies := []backtest.Strategy{
		backtest.Reactive{
			PerReplica: cfg.perReplica, Buffer: bufferAmt, Min: cfg.minRep, Max: cfg.maxRep,
		},
	}
	if cfg.staticRep > 0 {
		strategies = append(strategies, backtest.Static{Replicas: cfg.staticRep})
	}

	strategies = append(strategies, &backtest.Predictive{
		Label:          "predictive(SeasonalNaive)",
		Backend:        &forecast.SeasonalNaive{Season: cfg.season, Cycles: 3},
		Policy:         policyCfg,
		PerReplica:     cfg.perReplica,
		Resolution:     cfg.resolution,
		LeadSteps:      leadSteps,
		Context:        cfg.maxContext,
		TargetQuantile: cfg.targetQ,
		LowerQuantile:  cfg.lowerQ,
	})

	if cfg.timesfm != "" {
		strategies = append(strategies, &backtest.Predictive{
			Label: "predictive(TimesFM)",
			Backend: &forecast.TimesFM{
				Endpoint:   cfg.timesfm,
				MaxContext: cfg.maxContext,
				MaxHorizon: cfg.maxHorizon,
				Client:     &http.Client{Timeout: 60 * time.Second},
			},
			Policy:         policyCfg,
			PerReplica:     cfg.perReplica,
			Resolution:     cfg.resolution,
			LeadSteps:      leadSteps,
			Context:        cfg.maxContext,
			TargetQuantile: cfg.targetQ,
			LowerQuantile:  cfg.lowerQ,
		})
	}

	strategies = append(strategies, backtest.Oracle{
		PerReplica: cfg.perReplica, LeadSteps: leadSteps, Buffer: bufferAmt,
		Min: cfg.minRep, Max: cfg.maxRep, Full: series.Values,
	})

	fmt.Fprintf(os.Stderr, "scoring %d steps across %d strategies...\n",
		len(series.Values)-warmup, len(strategies))

	scores, err := backtest.RunAll(ctx, opts, strategies...)
	if err != nil {
		return err
	}

	/* Tune a reactive baseline to each predictive strategy's spend. Without
	 * this the report can only say "less unmet demand", which any strategy can
	 * achieve by provisioning more -- and would be an advertisement rather
	 * than an evaluation. */
	isoCost := map[string]backtest.IsoCost{}
	for _, s := range scores {
		if !strings.HasPrefix(s.Strategy, "predictive") {
			continue
		}
		fmt.Fprintf(os.Stderr, "matching a reactive baseline to %s (%.2f replicas)...\n",
			s.Strategy, s.AvgReplicas())
		iso, err := backtest.MatchCost(ctx, opts, s.AvgReplicas(), cfg.minRep, cfg.maxRep)
		if err != nil {
			return err
		}
		isoCost[s.Strategy] = iso
	}

	report := backtest.Report(scores, opts, isoCost)
	if cfg.out == "" {
		fmt.Print(report)
		return nil
	}
	if err := os.WriteFile(cfg.out, []byte(report), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s\n", cfg.out)
	return nil
}

// parseAmount accepts "5" or "10%".
func parseAmount(s string) (policy.Amount, error) {
	if s == "" {
		return policy.Amount{}, nil
	}
	if s[len(s)-1] == '%' {
		var v float64
		if _, err := fmt.Sscanf(s[:len(s)-1], "%g", &v); err != nil {
			return policy.Amount{}, fmt.Errorf("unparseable percentage %q", s)
		}
		return policy.Amount{Value: v, Percent: true}, nil
	}
	var v float64
	if _, err := fmt.Sscanf(s, "%g", &v); err != nil {
		return policy.Amount{}, fmt.Errorf("unparseable amount %q", s)
	}
	return policy.Amount{Value: v}, nil
}
