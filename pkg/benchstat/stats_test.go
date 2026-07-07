package benchstat

import (
	"math"
	"testing"
)

const epsilon = 1e-9

func almostEqual(a, b float64) bool {
	return math.Abs(a-b) <= epsilon
}

func TestPercentileEmpty(t *testing.T) {
	if got := Percentile(nil, 50); !math.IsNaN(got) {
		t.Errorf("Percentile(nil, 50) = %v, want NaN", got)
	}
	if got := Percentile([]float64{}, 0); !math.IsNaN(got) {
		t.Errorf("Percentile([], 0) = %v, want NaN", got)
	}
}

func TestPercentileKnownValues(t *testing.T) {
	tests := []struct {
		name   string
		sorted []float64
		p      float64
		want   float64
	}{
		{"single element p0", []float64{42}, 0, 42},
		{"single element p50", []float64{42}, 50, 42},
		{"single element p100", []float64{42}, 100, 42},
		{"two elements p0", []float64{10, 20}, 0, 10},
		{"two elements p50 interpolated", []float64{10, 20}, 50, 15},
		{"two elements p25 interpolated", []float64{10, 20}, 25, 12.5},
		{"two elements p100", []float64{10, 20}, 100, 20},
		{"exact rank hit p50", []float64{1, 2, 3, 4, 5}, 50, 3},
		{"exact rank hit p25", []float64{1, 2, 3, 4, 5}, 25, 2},
		{"exact rank hit p100", []float64{1, 2, 3, 4, 5}, 100, 5},
		{"interpolated rank p50", []float64{1, 2, 3, 4}, 50, 2.5},
		{"interpolated rank p90", []float64{1, 2, 3, 4, 5}, 90, 4.6},
		{"interpolated rank p99", []float64{1, 2, 3, 4, 5}, 99, 4.96},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Percentile(tt.sorted, tt.p)
			if !almostEqual(got, tt.want) {
				t.Errorf("Percentile(%v, %v) = %v, want %v", tt.sorted, tt.p, got, tt.want)
			}
		})
	}
}

func TestPercentileClampsOutOfRange(t *testing.T) {
	sorted := []float64{1, 2, 3}
	if got := Percentile(sorted, -10); !almostEqual(got, 1) {
		t.Errorf("Percentile(sorted, -10) = %v, want 1", got)
	}
	if got := Percentile(sorted, 150); !almostEqual(got, 3) {
		t.Errorf("Percentile(sorted, 150) = %v, want 3", got)
	}
}

func TestSummarizeEmpty(t *testing.T) {
	got := Summarize(nil)
	want := Summary{}
	if got != want {
		t.Errorf("Summarize(nil) = %+v, want zero-value Summary", got)
	}
}

func TestSummarizeKnownValues(t *testing.T) {
	samples := []float64{5, 1, 4, 2, 3}
	got := Summarize(samples)

	if got.N != 5 {
		t.Errorf("N = %d, want 5", got.N)
	}
	if !almostEqual(got.Mean, 3) {
		t.Errorf("Mean = %v, want 3", got.Mean)
	}
	if !almostEqual(got.Min, 1) {
		t.Errorf("Min = %v, want 1", got.Min)
	}
	if !almostEqual(got.Max, 5) {
		t.Errorf("Max = %v, want 5", got.Max)
	}
	if !almostEqual(got.P50, 3) {
		t.Errorf("P50 = %v, want 3", got.P50)
	}
	if !almostEqual(got.P90, 4.6) {
		t.Errorf("P90 = %v, want 4.6", got.P90)
	}
	if !almostEqual(got.P99, 4.96) {
		t.Errorf("P99 = %v, want 4.96", got.P99)
	}
}

func TestSummarizeDoesNotMutateInput(t *testing.T) {
	samples := []float64{5, 1, 4, 2, 3}
	want := []float64{5, 1, 4, 2, 3}
	_ = Summarize(samples)
	for i := range samples {
		if samples[i] != want[i] {
			t.Fatalf("input mutated: got %v, want %v", samples, want)
		}
	}
}

func TestSummarizeSingleSample(t *testing.T) {
	got := Summarize([]float64{7})
	want := Summary{N: 1, Mean: 7, Min: 7, Max: 7, P50: 7, P90: 7, P99: 7}
	if got != want {
		t.Errorf("Summarize([7]) = %+v, want %+v", got, want)
	}
}

func TestMarkdownTableGolden(t *testing.T) {
	headers := []string{"metric", "p50", "p99"}
	rows := [][]string{
		{"boot_ms", "120", "310"},
		{"resume_ms", "450", "980"},
	}
	got := MarkdownTable(headers, rows)
	want := "| metric | p50 | p99 |\n" +
		"| --- | --- | --- |\n" +
		"| boot_ms | 120 | 310 |\n" +
		"| resume_ms | 450 | 980 |\n"
	if got != want {
		t.Errorf("MarkdownTable golden mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestMarkdownTableNoRows(t *testing.T) {
	got := MarkdownTable([]string{"a", "b"}, nil)
	want := "| a | b |\n| --- | --- |\n"
	if got != want {
		t.Errorf("MarkdownTable with no rows:\ngot:\n%s\nwant:\n%s", got, want)
	}
}
