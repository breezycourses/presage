package controller

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/breezycourses/presage/internal/policy"
)

// Defaults applied when a field is omitted. They are here rather than only in
// kubebuilder markers so that an object created before a field existed, or by
// a client that bypasses defaulting, still reconciles predictably.
const (
	defaultResolution      = 5 * time.Minute
	defaultHistory         = 14 * 24 * time.Hour
	defaultInterval        = time.Minute
	defaultLeadTime        = 2 * time.Minute
	defaultLeadTimeMin     = 30 * time.Second
	defaultLeadTimeMax     = 15 * time.Minute
	defaultScaleDownWindow = 15 * time.Minute
	defaultTargetQuantile  = 0.9
	defaultDownQuantile    = 0.5
	defaultMaxSpread       = 0.25
	defaultAgonesMaxAge    = 5 * time.Minute

	// defaultHorizonFactor forecasts further than strictly needed. The extra
	// points cost nothing and make the projected curve useful on a dashboard,
	// which matters a great deal when explaining a surprising scale decision.
	defaultHorizonFactor = 4

	// maxGapRatio is the largest share of a signal window that may be
	// gap-filled before the series is refused. Judged against the total number
	// of steps, not the number of returned values.
	maxGapRatio = 0.5
)

// parseAmount converts an IntOrString into a policy Amount. Percentages must
// be written as a string ending in '%'; a bare string is rejected rather than
// guessed at, because "10" and "10%" differ by an order of magnitude in most
// realistic configurations.
func parseAmount(v *intstr.IntOrString, fallback policy.Amount) (policy.Amount, error) {
	if v == nil {
		return fallback, nil
	}
	switch v.Type {
	case intstr.Int:
		return policy.Amount{Value: float64(v.IntValue())}, nil
	case intstr.String:
		s := strings.TrimSpace(v.StrVal)
		if !strings.HasSuffix(s, "%") {
			// A bare string is rejected because "10" and "10%" differ by an
			// order of magnitude in most realistic configurations -- except
			// at zero, where they mean the same thing. Refusing "0" there is
			// pedantry that buys no safety and reads as a bug.
			if f, err := strconv.ParseFloat(s, 64); err == nil && f == 0 {
				return policy.Amount{}, nil
			}
			return policy.Amount{}, fmt.Errorf(
				"%q must be a number or a percentage like \"10%%\"", v.StrVal)
		}
		f, err := strconv.ParseFloat(strings.TrimSuffix(s, "%"), 64)
		if err != nil {
			return policy.Amount{}, fmt.Errorf("unparseable percentage %q: %w", v.StrVal, err)
		}
		if f < 0 {
			return policy.Amount{}, fmt.Errorf("percentage %q must not be negative", v.StrVal)
		}
		return policy.Amount{Value: f, Percent: true}, nil
	default:
		return policy.Amount{}, fmt.Errorf("unsupported value %v", v)
	}
}

// parseQuantile validates a quantile string. Zero and one are rejected: a
// backend cannot express them, and silently clamping would give the operator
// a service level they did not ask for.
func parseQuantile(s string, fallback float64) (float64, error) {
	if strings.TrimSpace(s) == "" {
		return fallback, nil
	}
	q, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("unparseable quantile %q: %w", s, err)
	}
	if q <= 0 || q >= 1 {
		return 0, fmt.Errorf("quantile %q must be strictly between 0 and 1", s)
	}
	return q, nil
}

// parseRatio validates a non-negative ratio such as a relative spread.
func parseRatio(s string, fallback float64) (float64, error) {
	if strings.TrimSpace(s) == "" {
		return fallback, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("unparseable ratio %q: %w", s, err)
	}
	if f < 0 {
		return 0, fmt.Errorf("ratio %q must not be negative", s)
	}
	return f, nil
}

func durOr(d *metav1.Duration, fallback time.Duration) time.Duration {
	if d == nil || d.Duration <= 0 {
		return fallback
	}
	return d.Duration
}

func boolOr(b *bool, fallback bool) bool {
	if b == nil {
		return fallback
	}
	return *b
}

// clampDuration bounds d to [lo, hi].
func clampDuration(d, lo, hi time.Duration) time.Duration {
	if d < lo {
		return lo
	}
	if d > hi {
		return hi
	}
	return d
}
