package runtimecheck

import (
	"testing"
	"time"
)

func TestMedian(t *testing.T) {
	tests := []struct {
		values []float64
		want   float64
	}{
		{nil, 0},
		{[]float64{0.5}, 0.5},
		{[]float64{0.9, 0.1, 0.5}, 0.5},
		// Parzysta liczba pomiarów: średnia dwóch środkowych, jak
		// statistics.median w dawnym skrypcie.
		{[]float64{0.4, 0.1, 0.2, 0.3}, 0.25},
	}
	for _, tt := range tests {
		if got := median(tt.values); got != tt.want {
			t.Errorf("median(%v) = %v, oczekiwano %v", tt.values, got, tt.want)
		}
	}
}

func TestDefaultLimits(t *testing.T) {
	// Żądanie w locie musi być krótsze niż service.shutdown_grace_seconds
	// konfiguracji przykładowej (3 s) — inaczej bramka byłaby czerwona
	// z definicji.
	if DefaultLimits.InflightDelay >= 3*time.Second {
		t.Errorf("InflightDelay %v nie mieści się w shutdown_grace_seconds", DefaultLimits.InflightDelay)
	}
	if got := seconds(10 * time.Second); got != "10" {
		t.Errorf("seconds = %q", got)
	}
}
