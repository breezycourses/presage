// Package metrics reads signal history from a Prometheus-compatible endpoint.
//
// The client speaks the plain HTTP query API rather than depending on the
// Prometheus Go client, so it works unchanged against Thanos, Mimir, Cortex,
// and VictoriaMetrics' vmselect.
package metrics

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client queries a Prometheus-compatible API.
type Client struct {
	Address     string
	BearerToken string
	HTTP        *http.Client
}

// NewClient builds a client with a sane timeout.
func NewClient(address, bearerToken string, insecureSkipVerify bool, timeout time.Duration) *Client {
	transport := http.DefaultTransport
	if insecureSkipVerify {
		transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // opt-in
		}
	}
	return &Client{
		Address:     strings.TrimRight(address, "/"),
		BearerToken: bearerToken,
		HTTP:        &http.Client{Timeout: timeout, Transport: transport},
	}
}

type apiResponse struct {
	Status    string          `json:"status"`
	Data      json.RawMessage `json:"data"`
	ErrorType string          `json:"errorType"`
	Error     string          `json:"error"`
}

type matrixData struct {
	ResultType string `json:"resultType"`
	Result     []struct {
		Metric map[string]string    `json:"metric"`
		Values [][2]json.RawMessage `json:"values"`
	} `json:"result"`
}

type vectorData struct {
	ResultType string `json:"resultType"`
	Result     []struct {
		Metric map[string]string  `json:"metric"`
		Value  [2]json.RawMessage `json:"value"`
	} `json:"result"`
}

// RangeResult is an evenly spaced series read from a range query.
type RangeResult struct {
	// Values are ordered oldest to newest, one per step. Gaps are filled by
	// carrying the previous value forward.
	Values []float64
	// Timestamps parallel to Values.
	Timestamps []time.Time
	// Gaps is how many steps had no sample and were filled or skipped.
	Gaps int
	// Steps is how many steps the range covered in total.
	//
	// Gaps must be judged against this, not against len(Values): a
	// carried-forward step appears in *both* Gaps and Values, so
	// Gaps/(Gaps+len(Values)) systematically understates how much of the
	// series is interpolation -- a 60%-gapped series scores 0.375 that way.
	Steps int
}

// QueryRange runs a range query and returns a single evenly spaced series.
//
// A forecasting model needs a regular grid, but Prometheus range queries can
// return sparse or ragged results (scrape gaps, restarts, a target that only
// just appeared). Rather than hand the model a series with holes -- which it
// would happily extrapolate nonsense from -- the result is resampled onto the
// exact requested grid and the number of filled steps is reported so the
// caller can refuse a series that is mostly interpolation.
func (c *Client) QueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) (*RangeResult, error) {
	if step <= 0 {
		return nil, fmt.Errorf("metrics: step must be > 0")
	}

	form := url.Values{}
	form.Set("query", query)
	form.Set("start", formatTime(start))
	form.Set("end", formatTime(end))
	form.Set("step", strconv.FormatFloat(step.Seconds(), 'f', -1, 64))

	raw, err := c.post(ctx, "/api/v1/query_range", form)
	if err != nil {
		return nil, err
	}

	var data matrixData
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("metrics: decode matrix: %w", err)
	}
	if len(data.Result) == 0 {
		return nil, fmt.Errorf("metrics: query returned no series: %s", truncate(query, 200))
	}
	if len(data.Result) > 1 {
		// Silently taking the first series would make the autoscaler's input
		// depend on map ordering. Better to fail and make the operator fix
		// the query.
		return nil, fmt.Errorf("metrics: query returned %d series, expected exactly 1; "+
			"add an aggregation such as sum() or a tighter label selector", len(data.Result))
	}

	// Index the returned samples by their step slot.
	samples := make(map[int64]float64, len(data.Result[0].Values))
	for _, pair := range data.Result[0].Values {
		ts, err := parseTimestamp(pair[0])
		if err != nil {
			return nil, err
		}
		v, err := parseSampleValue(pair[1])
		if err != nil {
			return nil, err
		}
		if math.IsNaN(v) {
			continue // treat NaN as a gap rather than poisoning the series
		}
		samples[ts.Unix()] = v
	}

	steps := int(end.Sub(start)/step) + 1
	if steps < 1 {
		return nil, fmt.Errorf("metrics: empty time range")
	}

	out := &RangeResult{
		Values:     make([]float64, 0, steps),
		Timestamps: make([]time.Time, 0, steps),
		Steps:      steps,
	}
	var last float64
	var haveLast bool
	for i := 0; i < steps; i++ {
		ts := start.Add(time.Duration(i) * step)
		v, ok := samples[ts.Unix()]
		switch {
		case ok:
			last, haveLast = v, true
		case haveLast:
			v = last
			out.Gaps++
		default:
			// Leading gap: nothing to carry forward yet. Skip rather than
			// inventing a zero, which would look like real idle traffic.
			out.Gaps++
			continue
		}
		out.Values = append(out.Values, v)
		out.Timestamps = append(out.Timestamps, ts)
	}

	if len(out.Values) == 0 {
		return nil, fmt.Errorf("metrics: query produced no usable samples over %s..%s",
			start.Format(time.RFC3339), end.Format(time.RFC3339))
	}
	return out, nil
}

// QueryScalar runs an instant query expected to yield exactly one value.
func (c *Client) QueryScalar(ctx context.Context, query string, at time.Time) (float64, error) {
	form := url.Values{}
	form.Set("query", query)
	form.Set("time", formatTime(at))

	raw, err := c.post(ctx, "/api/v1/query", form)
	if err != nil {
		return 0, err
	}

	var data vectorData
	if err := json.Unmarshal(raw, &data); err != nil {
		return 0, fmt.Errorf("metrics: decode vector: %w", err)
	}
	if len(data.Result) == 0 {
		return 0, fmt.Errorf("metrics: query returned no samples: %s", truncate(query, 200))
	}
	if len(data.Result) > 1 {
		return 0, fmt.Errorf("metrics: query returned %d samples, expected exactly 1", len(data.Result))
	}
	return parseSampleValue(data.Result[0].Value[1])
}

func (c *Client) post(ctx context.Context, path string, form url.Values) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Address+path,
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("metrics: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if c.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.BearerToken)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("metrics: query %s: %w", c.Address, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("metrics: read response: %w", err)
	}

	var api apiResponse
	if err := json.Unmarshal(body, &api); err != nil {
		return nil, fmt.Errorf("metrics: %s returned %s with an undecodable body: %s",
			c.Address, resp.Status, truncate(string(body), 256))
	}
	if api.Status != "success" {
		return nil, fmt.Errorf("metrics: query failed (%s): %s", api.ErrorType, api.Error)
	}
	return api.Data, nil
}

func formatTime(t time.Time) string {
	return strconv.FormatFloat(float64(t.Unix()), 'f', -1, 64)
}

// parseTimestamp reads a Prometheus sample timestamp, which is a JSON number
// of seconds, possibly fractional.
func parseTimestamp(raw json.RawMessage) (time.Time, error) {
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return time.Time{}, fmt.Errorf("metrics: bad timestamp %s: %w", raw, err)
	}
	sec := int64(f)
	return time.Unix(sec, int64((f-float64(sec))*float64(time.Second))), nil
}

// parseSampleValue reads a Prometheus sample value, which is a JSON *string*
// holding a float, and may be "NaN", "+Inf", or "-Inf".
func parseSampleValue(raw json.RawMessage) (float64, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0, fmt.Errorf("metrics: bad sample value %s: %w", raw, err)
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("metrics: unparseable sample value %q: %w", s, err)
	}
	return v, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
