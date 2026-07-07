// Package benchstat provides tiny order-statistics helpers and a markdown
// table renderer used by EmberVM benchmark tooling. It is intentionally
// dependency-free (pure stdlib) and portable across darwin and linux.
package benchstat

import (
	"math"
	"sort"
	"strings"
)

// Summary holds order statistics for a series of samples.
type Summary struct {
	N    int
	Mean float64
	Min  float64
	Max  float64
	P50  float64
	P90  float64
	P99  float64
}

// Summarize computes Summary over samples. It does not mutate the input.
// Percentiles use linear interpolation between closest ranks.
// Returns zero-value Summary when samples is empty.
func Summarize(samples []float64) Summary {
	if len(samples) == 0 {
		return Summary{}
	}

	sorted := make([]float64, len(samples))
	copy(sorted, samples)
	sort.Float64s(sorted)

	sum := 0.0
	for _, v := range sorted {
		sum += v
	}

	return Summary{
		N:    len(sorted),
		Mean: sum / float64(len(sorted)),
		Min:  sorted[0],
		Max:  sorted[len(sorted)-1],
		P50:  Percentile(sorted, 50),
		P90:  Percentile(sorted, 90),
		P99:  Percentile(sorted, 99),
	}
}

// Percentile returns the p-th percentile (0 <= p <= 100) of sorted
// (ascending) samples using linear interpolation. Panics if unsorted input
// is NOT required to be detected; callers must pass sorted data.
// Returns NaN for empty input.
func Percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return math.NaN()
	}
	if p < 0 {
		p = 0
	}
	if p > 100 {
		p = 100
	}

	// Linear interpolation between closest ranks:
	// rank spans [0, n-1] so that p=0 maps to the minimum and p=100 to the
	// maximum.
	rank := p / 100 * float64(len(sorted)-1)
	lo := int(math.Floor(rank))
	hi := int(math.Ceil(rank))
	if lo == hi {
		return sorted[lo]
	}
	frac := rank - float64(lo)
	return sorted[lo] + frac*(sorted[hi]-sorted[lo])
}

// MarkdownTable renders a GitHub-flavored markdown table.
func MarkdownTable(headers []string, rows [][]string) string {
	var b strings.Builder

	b.WriteString("| ")
	b.WriteString(strings.Join(headers, " | "))
	b.WriteString(" |\n")

	sep := make([]string, len(headers))
	for i := range sep {
		sep[i] = "---"
	}
	b.WriteString("| ")
	b.WriteString(strings.Join(sep, " | "))
	b.WriteString(" |\n")

	for _, row := range rows {
		b.WriteString("| ")
		b.WriteString(strings.Join(row, " | "))
		b.WriteString(" |\n")
	}

	return b.String()
}
