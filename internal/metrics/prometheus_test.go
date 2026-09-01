package metrics

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func serveMatrix(t *testing.T, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &Client{Address: srv.URL, HTTP: srv.Client()}
}

func matrix(samples ...string) string {
	return fmt.Sprintf(`{"status":"success","data":{"resultType":"matrix","result":[
		{"metric":{"fleet":"lobby"},"values":[%s]}]}}`, strings.Join(samples, ","))
}

func TestQueryRange_FillsGapsByCarryingForward(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	step := time.Minute
	// Steps 0, 1, 3, 4 present; step 2 missing.
	c := serveMatrix(t, matrix(
		fmt.Sprintf(`[%d,"10"]`, base.Unix()),
		fmt.Sprintf(`[%d,"20"]`, base.Add(step).Unix()),
		fmt.Sprintf(`[%d,"40"]`, base.Add(3*step).Unix()),
		fmt.Sprintf(`[%d,"50"]`, base.Add(4*step).Unix()),
	))

	got, err := c.QueryRange(context.Background(), "q", base, base.Add(4*step), step)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []float64{10, 20, 20, 40, 50}
	if len(got.Values) != len(want) {
		t.Fatalf("want %d values, got %d: %v", len(want), len(got.Values), got.Values)
	}
	for i := range want {
		if got.Values[i] != want[i] {
			t.Fatalf("step %d: got %v want %v (%v)", i, got.Values[i], want[i], got.Values)
		}
	}
	if got.Gaps != 1 {
		t.Fatalf("expected 1 reported gap, got %d", got.Gaps)
	}
	if got.Steps != 5 {
		t.Fatalf("expected 5 steps, got %d", got.Steps)
	}
}

// TestQueryRange_GapRatioDenominator pins the denominator callers must use.
// A carried-forward step appears in both Gaps and Values, so measuring
// interpolation as Gaps/(Gaps+len(Values)) understates it badly: this series
// is 60% filled but scores 0.375 that way.
func TestQueryRange_GapRatioDenominator(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	step := time.Minute

	samples := []string{fmt.Sprintf(`[%d,"10"]`, base.Unix())}
	for i := 1; i <= 3; i++ { // real samples at 0, 5, 6, 7 of 10 steps
		if i >= 1 && i <= 3 {
			samples = append(samples, fmt.Sprintf(`[%d,"20"]`, base.Add(time.Duration(4+i)*step).Unix()))
		}
	}
	c := serveMatrix(t, matrix(samples...))

	got, err := c.QueryRange(context.Background(), "q", base, base.Add(9*step), step)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Steps != 10 {
		t.Fatalf("expected 10 steps, got %d", got.Steps)
	}
	if got.Gaps != 6 {
		t.Fatalf("expected 6 gaps, got %d", got.Gaps)
	}

	correct := float64(got.Gaps) / float64(got.Steps)
	naive := float64(got.Gaps) / float64(got.Gaps+len(got.Values))
	if correct <= 0.5 {
		t.Fatalf("correct ratio %.3f should exceed 0.5", correct)
	}
	if naive > 0.5 {
		t.Fatalf("the naive ratio %.3f was supposed to understate this", naive)
	}
}

func TestQueryRange_SkipsLeadingGapRatherThanInventingZero(t *testing.T) {
	// A leading gap has nothing to carry forward. Emitting 0 would look like
	// genuine idle traffic to the model and drag the forecast down.
	base := time.Unix(1_700_000_000, 0).UTC()
	step := time.Minute
	c := serveMatrix(t, matrix(
		fmt.Sprintf(`[%d,"30"]`, base.Add(2*step).Unix()),
		fmt.Sprintf(`[%d,"40"]`, base.Add(3*step).Unix()),
	))

	got, err := c.QueryRange(context.Background(), "q", base, base.Add(3*step), step)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Values) != 2 || got.Values[0] != 30 {
		t.Fatalf("expected the leading gap to be dropped, got %v", got.Values)
	}
	if got.Gaps != 2 {
		t.Fatalf("expected 2 reported gaps, got %d", got.Gaps)
	}
}

func TestQueryRange_NaNIsTreatedAsAGap(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	step := time.Minute
	c := serveMatrix(t, matrix(
		fmt.Sprintf(`[%d,"10"]`, base.Unix()),
		fmt.Sprintf(`[%d,"NaN"]`, base.Add(step).Unix()),
		fmt.Sprintf(`[%d,"30"]`, base.Add(2*step).Unix()),
	))

	got, err := c.QueryRange(context.Background(), "q", base, base.Add(2*step), step)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []float64{10, 10, 30}
	for i := range want {
		if got.Values[i] != want[i] {
			t.Fatalf("step %d: got %v want %v", i, got.Values[i], want[i])
		}
	}
}

func TestQueryRange_RejectsMultipleSeries(t *testing.T) {
	// Picking one of several series would make the autoscaler's input depend
	// on response ordering.
	base := time.Unix(1_700_000_000, 0).UTC()
	c := serveMatrix(t, fmt.Sprintf(`{"status":"success","data":{"resultType":"matrix","result":[
		{"metric":{"fleet":"a"},"values":[[%d,"1"]]},
		{"metric":{"fleet":"b"},"values":[[%d,"2"]]}]}}`, base.Unix(), base.Unix()))

	_, err := c.QueryRange(context.Background(), "q", base, base.Add(time.Minute), time.Minute)
	if err == nil || !strings.Contains(err.Error(), "expected exactly 1") {
		t.Fatalf("expected a multi-series rejection, got %v", err)
	}
}

func TestQueryRange_PropagatesAPIError(t *testing.T) {
	c := serveMatrix(t, `{"status":"error","errorType":"bad_data","error":"parse error"}`)
	_, err := c.QueryRange(context.Background(), "q",
		time.Unix(0, 0), time.Unix(60, 0), time.Minute)
	if err == nil || !strings.Contains(err.Error(), "parse error") {
		t.Fatalf("expected the API error to surface, got %v", err)
	}
}

func TestQueryScalar(t *testing.T) {
	c := serveMatrix(t, `{"status":"success","data":{"resultType":"vector","result":[
		{"metric":{},"value":[1700000000,"123.5"]}]}}`)
	got, err := c.QueryScalar(context.Background(), "q", time.Unix(1_700_000_000, 0))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 123.5 {
		t.Fatalf("want 123.5, got %v", got)
	}
}
