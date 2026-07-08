// Package prewarm predicts a paused sandbox's next wake so the lifecycle
// engine can pull its working set back to the node before the resume
// arrives — the Azure "Serverless in the Wild" (ATC'20) histogram policy
// adapted to per-sandbox wake intervals (docs/zh/04 #5). Deliberately
// conservative: with too little history or too much variance it predicts
// nothing and the TTLs act as the fixed keep-alive fallback (the paper's
// ARIMA fallback for sparse data is deferred, ADR-0004).
package prewarm

import (
	"math"
	"sort"
	"time"
)

const (
	// MinSamples is the fewest wake intervals worth predicting from.
	MinSamples = 3
	// MaxCV is the coefficient-of-variation ceiling: above it the wake
	// pattern is noise, not rhythm, and predicting would thrash the cache.
	MaxCV = 2.0
	// windowPercentile places the pre-warm window start: P5 of the interval
	// distribution means 95% of observed wakes came later than this.
	windowPercentile = 0.05
)

// PredictNext returns when the sandbox paused at pausedAt is expected to
// wake, based on its past pause→resume intervals. ok=false means "no
// prediction" (insufficient or too-noisy history).
func PredictNext(pausedAt time.Time, intervals []time.Duration) (time.Time, bool) {
	if len(intervals) < MinSamples {
		return time.Time{}, false
	}
	mean, std := meanStd(intervals)
	if mean <= 0 || std/mean > MaxCV {
		return time.Time{}, false
	}
	return pausedAt.Add(percentile(intervals, windowPercentile)), true
}

// ShouldPrewarm reports whether the engine should pull the working set now:
// the predicted wake is within lead, and not already behind us by more than
// one lead (a long-overdue prediction is stale, not urgent).
func ShouldPrewarm(now, pausedAt time.Time, intervals []time.Duration, lead time.Duration) bool {
	predicted, ok := PredictNext(pausedAt, intervals)
	if !ok {
		return false
	}
	return !now.Before(predicted.Add(-lead)) && now.Before(predicted.Add(lead))
}

func meanStd(ds []time.Duration) (mean, std float64) {
	for _, d := range ds {
		mean += float64(d)
	}
	mean /= float64(len(ds))
	var varsum float64
	for _, d := range ds {
		diff := float64(d) - mean
		varsum += diff * diff
	}
	return mean, math.Sqrt(varsum / float64(len(ds)))
}

func percentile(ds []time.Duration, p float64) time.Duration {
	sorted := append([]time.Duration(nil), ds...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
