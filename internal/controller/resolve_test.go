package controller

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/GrowlyX/presage/internal/policy"
)

func TestParseAmount(t *testing.T) {
	fallback := policy.Amount{Value: 7}
	tests := []struct {
		name    string
		in      *intstr.IntOrString
		want    policy.Amount
		wantErr bool
	}{
		{"nil uses fallback", nil, fallback, false},
		{"int", ptr(intstr.FromInt32(5)), policy.Amount{Value: 5}, false},
		{"percent", ptr(intstr.FromString("15%")), policy.Amount{Value: 15, Percent: true}, false},
		{"fractional percent", ptr(intstr.FromString("12.5%")), policy.Amount{Value: 12.5, Percent: true}, false},
		{"whitespace tolerated", ptr(intstr.FromString(" 20% ")), policy.Amount{Value: 20, Percent: true}, false},
		// "10" and "10%" differ by an order of magnitude in most real configs,
		// so a bare string must be an error rather than a guess.
		{"bare string rejected", ptr(intstr.FromString("10")), policy.Amount{}, true},
		{"garbage rejected", ptr(intstr.FromString("abc%")), policy.Amount{}, true},
		{"negative rejected", ptr(intstr.FromString("-5%")), policy.Amount{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseAmount(tt.in, fallback)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseQuantile(t *testing.T) {
	tests := []struct {
		in      string
		want    float64
		wantErr bool
	}{
		{"", 0.9, false},
		{"0.9", 0.9, false},
		{"0.5", 0.5, false},
		{"0.99", 0.99, false},
		{"0", 0, true}, // not expressible
		{"1", 0, true}, // not expressible
		{"1.5", 0, true},
		{"-0.1", 0, true},
		{"p90", 0, true},
	}
	for _, tt := range tests {
		got, err := parseQuantile(tt.in, 0.9)
		if tt.wantErr {
			if err == nil {
				t.Errorf("parseQuantile(%q): expected an error", tt.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseQuantile(%q): unexpected error %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("parseQuantile(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestClampDuration(t *testing.T) {
	lo, hi := time.Second, time.Minute
	if got := clampDuration(0, lo, hi); got != lo {
		t.Errorf("want clamp up to %s, got %s", lo, got)
	}
	if got := clampDuration(time.Hour, lo, hi); got != hi {
		t.Errorf("want clamp down to %s, got %s", hi, got)
	}
	if got := clampDuration(30*time.Second, lo, hi); got != 30*time.Second {
		t.Errorf("want passthrough, got %s", got)
	}
}

func ptr[T any](v T) *T { return &v }
