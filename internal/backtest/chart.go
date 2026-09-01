package backtest

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

/* Charts are emitted as hand-built SVG rather than through a plotting library.
 * A backtest report is a build artefact that has to render identically on
 * anyone's machine and be reviewable as a diff; a charting dependency buys
 * axis niceties in exchange for a font stack, a raster step, and a third-party
 * upgrade treadmill. The palette matches the project's branding so a chart
 * pasted into the README does not look borrowed. */

const (
	chartBG     = "#141416"
	chartPanel  = "#1B1C1E"
	chartGrid   = "#2A2C30"
	chartText   = "#9A9DA4"
	chartBright = "#F4F5F7"
	chartShort  = "#D96A6A"
)

// seriesColors are assigned in order; the required-capacity line is drawn
// separately in a neutral tone so strategies never collide with it.
var seriesColors = []string{"#7FA9F0", "#7ED0A7", "#E0B15C", "#C48BE0", "#E08A8A"}

// TimelineOptions controls the timeline chart.
type TimelineOptions struct {
	Title string
	// Width and Height in pixels. Zero uses a README-friendly default.
	Width, Height int
	// MaxPoints downsamples the x axis. A chart with more points than pixels
	// is not more informative, only larger.
	MaxPoints int

	// LastSteps, when non-zero, charts only the final N steps. A fortnight of
	// daily cycles compressed into one axis proves the strategies track
	// demand but hides the thing worth seeing -- whether a strategy moved
	// *before* the ramp or after it. That is visible only close up.
	LastSteps int
	// StartOffset shifts the x-axis labels so a zoomed window still reports
	// its true position in the run.
	StartOffset int
}

// Timeline renders provisioned capacity against required capacity over time.
//
// This is the chart worth looking at: a table of averages hides *when* a
// strategy was short. Being short for ten minutes during every morning ramp
// and being short for a scattered ten minutes of noise produce the same
// summary row and are completely different problems.
func Timeline(scores []Score, opts Options, chart TimelineOptions) string {
	if len(scores) == 0 || len(scores[0].Trace.Required) == 0 {
		return ""
	}
	w, h := chart.Width, chart.Height
	if w == 0 {
		w = 1200
	}
	if h == 0 {
		h = 460
	}
	maxPoints := chart.MaxPoints
	if maxPoints == 0 {
		maxPoints = w
	}

	const (
		padL = 66.0
		padR = 24.0
		padT = 58.0
		padB = 62.0
	)
	plotW := float64(w) - padL - padR
	plotH := float64(h) - padT - padB

	window := func(v []int32) []float64 {
		f := toFloat(v)
		if chart.LastSteps > 0 && len(f) > chart.LastSteps {
			f = f[len(f)-chart.LastSteps:]
		}
		return f
	}

	required := downsampleMax(window(scores[0].Trace.Required), maxPoints)
	n := len(required)

	// A common y scale across every series, or the lines are not comparable.
	yMax := maxOf(required)
	lines := make([][]float64, 0, len(scores))
	for _, s := range scores {
		v := downsampleMax(window(s.Trace.Provisioned), maxPoints)
		lines = append(lines, v)
		yMax = math.Max(yMax, maxOf(v))
	}
	yMax = niceCeil(yMax * 1.08)

	x := func(i int) float64 {
		if n <= 1 {
			return padL
		}
		return padL + plotW*float64(i)/float64(n-1)
	}
	y := func(v float64) float64 { return padT + plotH*(1-v/yMax) }

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d">`, w, h, w, h)
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="%s"/>`, w, h, chartBG)
	fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" fill="%s"/>`,
		padL, padT, plotW, plotH, chartPanel)

	if chart.Title != "" {
		b.WriteString(label(chart.Title, padL, 30, 17, "semibold", chartBright, AnchorStart))
	}

	// y grid and labels
	for i := 0; i <= 4; i++ {
		v := yMax * float64(i) / 4
		yy := y(v)
		fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="%s" stroke-width="1"/>`,
			padL, yy, padL+plotW, yy, chartGrid)
		b.WriteString(label(fmt.Sprintf("%.0f", v), padL-10, yy+4, 12, "regular", chartText, AnchorEnd))
	}
	b.WriteString(labelRotated("replicas", 16, padT+plotH/2, 12, -90, "regular", chartText, AnchorMiddle))

	// x labels in elapsed time
	shown := scores[0].Steps
	if chart.LastSteps > 0 && chart.LastSteps < shown {
		shown = chart.LastSteps
	}
	total := time.Duration(shown) * opts.Resolution
	offset := time.Duration(chart.StartOffset) * opts.Resolution
	for i := 0; i <= 6; i++ {
		frac := float64(i) / 6
		xx := padL + plotW*frac
		fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="%s" stroke-width="1"/>`,
			xx, padT, xx, padT+plotH, chartGrid)
		b.WriteString(label(axisLabel(offset+time.Duration(frac*float64(total)), total/6),
			xx, padT+plotH+20, 12, "regular", chartText, AnchorMiddle))
	}

	// Shortfall of the *first* strategy, shaded. Shading every strategy would
	// be unreadable; the first is the one the caller ordered first.
	if short := shortfallBands(required, lines[0], x, y, padT+plotH); short != "" {
		fmt.Fprintf(&b, `<g fill="%s" opacity="0.28">%s</g>`, chartShort, short)
	}

	// Required capacity: the thing every strategy is trying to cover.
	fmt.Fprintf(&b, `<path d="%s" fill="none" stroke="%s" stroke-width="1.6" stroke-dasharray="5 4" opacity="0.85"/>`,
		path(required, x, y), chartBright)

	for i, v := range lines {
		fmt.Fprintf(&b, `<path d="%s" fill="none" stroke="%s" stroke-width="2" stroke-linejoin="round" opacity="0.95"/>`,
			path(v, x, y), seriesColors[i%len(seriesColors)])
	}

	// Legend
	lx := padL
	ly := float64(h) - 18
	fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="%s" stroke-width="1.6" stroke-dasharray="5 4"/>`,
		lx, ly-4, lx+22, ly-4, chartBright)
	b.WriteString(label("required", lx+28, ly, 12, "regular", chartText, AnchorStart))
	lx += 28 + textWidth("required", 12, "regular") + 26
	for i, s := range scores {
		fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="%s" stroke-width="2.4"/>`,
			lx, ly-4, lx+22, ly-4, seriesColors[i%len(seriesColors)])
		b.WriteString(label(s.Strategy, lx+28, ly, 12, "regular", chartText, AnchorStart))
		lx += 28 + textWidth(s.Strategy, 12, "regular") + 26
	}

	b.WriteString(`</svg>`)
	return b.String()
}

// Tradeoff renders cost against service quality, which is the only honest way
// to compare autoscaling strategies: any of them can improve one axis by
// giving up the other, so the useful picture is where each one lands.
func Tradeoff(scores []Score, title string) string {
	if len(scores) == 0 {
		return ""
	}
	const w, h = 780.0, 420.0
	const padL, padR, padT, padB = 70.0, 250.0, 56.0, 56.0
	plotW, plotH := w-padL-padR, h-padT-padB

	xMax, yMax := 0.0, 0.0
	for _, s := range scores {
		xMax = math.Max(xMax, s.AvgReplicas())
		yMax = math.Max(yMax, s.UnmetStepFraction()*100)
	}
	xMax = niceCeil(xMax * 1.15)
	yMax = niceCeil(math.Max(yMax*1.25, 0.5))

	x := func(v float64) float64 { return padL + plotW*v/xMax }
	y := func(v float64) float64 { return padT + plotH*(1-v/yMax) }

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%.0f" height="%.0f" viewBox="0 0 %.0f %.0f">`, w, h, w, h)
	fmt.Fprintf(&b, `<rect width="%.0f" height="%.0f" fill="%s"/>`, w, h, chartBG)
	fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" fill="%s"/>`, padL, padT, plotW, plotH, chartPanel)
	if title != "" {
		b.WriteString(label(title, padL, 30, 16, "semibold", chartBright, AnchorStart))
	}

	for i := 0; i <= 4; i++ {
		v := yMax * float64(i) / 4
		yy := y(v)
		fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="%s"/>`, padL, yy, padL+plotW, yy, chartGrid)
		b.WriteString(label(fmt.Sprintf("%.1f%%", v), padL-8, yy+4, 11, "regular", chartText, AnchorEnd))

		u := xMax * float64(i) / 4
		xx := x(u)
		fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="%s"/>`, xx, padT, xx, padT+plotH, chartGrid)
		b.WriteString(label(fmt.Sprintf("%.0f", u), xx, padT+plotH+18, 11, "regular", chartText, AnchorMiddle))
	}
	b.WriteString(label("average replicas (cost)", padL+plotW/2, h-14, 12, "regular", chartText, AnchorMiddle))
	b.WriteString(labelRotated("steps short (worse)", 16, padT+plotH/2, 12, -90, "regular", chartText, AnchorMiddle))

	// Bottom-left is better on both axes; say so rather than making the reader
	// work out which direction is good.
	b.WriteString(`<g opacity="0.75">` +
		label("\u2190 cheaper, fewer shortfalls", padL+6, padT+plotH-8, 11, "regular", chartText, AnchorStart) + `</g>`)

	for i, s := range scores {
		cx, cy := x(s.AvgReplicas()), y(s.UnmetStepFraction()*100)
		color := seriesColors[i%len(seriesColors)]
		fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="6" fill="%s"/>`, cx, cy, color)
		fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="11" fill="none" stroke="%s" opacity="0.35"/>`, cx, cy, color)
		// Labels to the right of the plot so overlapping points stay readable.
		ly := padT + 18 + float64(i)*22
		fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="%s" opacity="0.35"/>`,
			cx+12, cy, padL+plotW+12, ly-4, color)
		fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="4" fill="%s"/>`, padL+plotW+18, ly-4, color)
		b.WriteString(label(s.Strategy, padL+plotW+26, ly, 11, "regular", chartText, AnchorStart))
	}

	b.WriteString(`</svg>`)
	return b.String()
}

// --- helpers ---------------------------------------------------------------

func path(v []float64, x func(int) float64, y func(float64) float64) string {
	var b strings.Builder
	for i, p := range v {
		cmd := "L"
		if i == 0 {
			cmd = "M"
		}
		fmt.Fprintf(&b, "%s%.1f,%.1f", cmd, x(i), y(p))
	}
	return b.String()
}

// shortfallBands shades the regions where provisioned fell below required.
func shortfallBands(required, provisioned []float64, x func(int) float64, y func(float64) float64, base float64) string {
	var b strings.Builder
	i := 0
	for i < len(required) {
		if i >= len(provisioned) || provisioned[i] >= required[i] {
			i++
			continue
		}
		start := i
		for i < len(required) && i < len(provisioned) && provisioned[i] < required[i] {
			i++
		}
		x0, x1 := x(start), x(i-1)
		if x1-x0 < 1 {
			x1 = x0 + 1 // a single short step must still be visible
		}
		fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f"/>`,
			x0, y(maxOf(required[start:i])), x1-x0, base-y(maxOf(required[start:i])))
	}
	return b.String()
}

func toFloat(v []int32) []float64 {
	out := make([]float64, len(v))
	for i, x := range v {
		out[i] = float64(x)
	}
	return out
}

// downsampleMax reduces to at most n points, keeping the maximum of each
// bucket. Taking the mean would smooth away exactly the peaks that decide
// whether a strategy was ever short.
func downsampleMax(v []float64, n int) []float64 {
	if n <= 0 || len(v) <= n {
		return v
	}
	out := make([]float64, n)
	for i := range out {
		lo := i * len(v) / n
		hi := (i + 1) * len(v) / n
		if hi <= lo {
			hi = lo + 1
		}
		if hi > len(v) {
			hi = len(v)
		}
		out[i] = maxOf(v[lo:hi])
	}
	return out
}

func maxOf(v []float64) float64 {
	m := 0.0
	for _, x := range v {
		m = math.Max(m, x)
	}
	return m
}

// niceCeil rounds up to a readable axis bound.
func niceCeil(v float64) float64 {
	if v <= 0 {
		return 1
	}
	mag := math.Pow(10, math.Floor(math.Log10(v)))
	for _, step := range []float64{1, 2, 2.5, 5, 10} {
		if b := step * mag; b >= v {
			return b
		}
	}
	return 10 * mag
}

// axisLabel renders a tick at a granularity matched to the tick spacing.
// Labelling a three-day window in whole days repeats "day 12" twice in a row
// and tells the reader nothing about where they are inside it.
func axisLabel(at, spacing time.Duration) string {
	if spacing >= 24*time.Hour {
		return elapsedLabel(at)
	}
	day := int(at.Hours()/24) + 1
	hour := int(at.Hours()) % 24
	return fmt.Sprintf("d%d %02d:00", day, hour)
}

// elapsedLabel renders elapsed time at a granularity a reader can hold in
// their head. "56h0m0s" is technically correct and useless on a two-week axis.
func elapsedLabel(d time.Duration) string {
	switch {
	case d == 0:
		return "0"
	case d >= 72*time.Hour:
		return fmt.Sprintf("day %d", int(d.Hours()/24)+1)
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return d.Round(time.Minute).String()
	}
}

func escape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

var _ = sort.Float64s

var _ = escape
