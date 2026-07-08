package prewarm

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)

func mins(ns ...int) []time.Duration {
	out := make([]time.Duration, len(ns))
	for i, n := range ns {
		out[i] = time.Duration(n) * time.Minute
	}
	return out
}

func TestPredictNextRegularPattern(t *testing.T) {
	// Wakes every ~30 minutes: prediction at P5 of the distribution.
	predicted, ok := PredictNext(t0, mins(28, 30, 31, 29, 32))
	if !ok {
		t.Fatal("regular pattern produced no prediction")
	}
	if want := t0.Add(28 * time.Minute); !predicted.Equal(want) {
		t.Fatalf("predicted = %v, want %v (P5 = min of 5 samples)", predicted, want)
	}
}

func TestPredictNextTooFewSamples(t *testing.T) {
	if _, ok := PredictNext(t0, mins(30, 31)); ok {
		t.Fatal("2 samples produced a prediction (MinSamples=3)")
	}
	if _, ok := PredictNext(t0, nil); ok {
		t.Fatal("no samples produced a prediction")
	}
}

func TestPredictNextNoisyPattern(t *testing.T) {
	// Heavy-tailed history (CV > 2): five instant wakes and one six-week
	// sleep — no rhythm worth predicting.
	noisy := []time.Duration{time.Minute, time.Minute, time.Minute, time.Minute, time.Minute, 1000 * time.Hour}
	if _, ok := PredictNext(t0, noisy); ok {
		t.Fatal("noisy pattern produced a prediction")
	}
}

func TestShouldPrewarmWindow(t *testing.T) {
	intervals := mins(30, 30, 30, 30)
	lead := 5 * time.Minute
	// Predicted wake: t0+30m; window [25m, 35m).
	cases := []struct {
		at   time.Duration
		want bool
	}{
		{10 * time.Minute, false}, // far before the window
		{24 * time.Minute, false},
		{25 * time.Minute, true}, // window opens at predicted-lead
		{30 * time.Minute, true},
		{34 * time.Minute, true},
		{36 * time.Minute, false}, // stale: wake already overdue past lead
	}
	for _, c := range cases {
		if got := ShouldPrewarm(t0.Add(c.at), t0, intervals, lead); got != c.want {
			t.Errorf("ShouldPrewarm at +%v = %v, want %v", c.at, got, c.want)
		}
	}
}

func TestShouldPrewarmNoHistory(t *testing.T) {
	if ShouldPrewarm(t0.Add(time.Hour), t0, mins(30), 5*time.Minute) {
		t.Fatal("prewarmed with one sample")
	}
}
