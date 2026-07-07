// Command genreport renders the EmberVM M0 baseline benchmark report from
// the JSON artifacts produced by the test/bench suite.
//
// Inputs (all optional; a missing or unparsable file renders its section as
// "no data collected in this run" and is never an error):
//
//	env.json          environment triple + host capability probe
//	restore-*.json    one file per restore-latency matrix cell
//	uffdio-copy.json  raw UFFDIO_COPY throughput sweep
//	zfs-compare.json  raw-file-on-dataset vs zvol fio comparison
//
// Output: a single markdown report (default results/REPORT.md).
//
// Per docs/zh/06 §2 every published benchmark must carry its environment
// triple (bare-metal|nested@vendor, CPU model, kernel version); data from
// nested environments is functional reference only and README performance
// claims may cite bare-metal data only. The generator enforces this by
// stamping the triple into the report header and emitting a bold disclaimer
// whenever the environment is not bare metal.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/embervm/embervm/pkg/benchstat"
)

const noData = "_no data collected in this run_"

// Documented budget lines from docs/zh/04 §2 (end-to-end latency budget
// model, hot restore path).
const (
	budgetAPIMaxMs         = 20.0  // snapfile load + device rebuild: 5-20ms
	budgetInteractiveMaxMs = 500.0 // hot restore interactive target: P50 < 500ms
)

// REAP reference band for uffd page delivery (docs/zh/04 §2/§3): fault-path
// floor 24.6us/page ~= 260 MiB/s, eager sequential working-set read 533 MB/s.
const (
	reapBandLowMBs  = 260.0
	reapBandHighMBs = 533.0
)

// envInfo mirrors env.json.
type envInfo struct {
	EnvType           string `json:"env_type"`
	CPUModel          string `json:"cpu_model"`
	Kernel            string `json:"kernel"`
	CPUs              int    `json:"cpus"`
	MemMB             int    `json:"mem_mb"`
	DiskFreeGB        int    `json:"disk_free_gb"`
	KVM               bool   `json:"kvm"`
	VMX               bool   `json:"vmx"`
	EPT               string `json:"ept"`
	UnrestrictedGuest string `json:"unrestricted_guest"`
	Cgroup2           bool   `json:"cgroup2"`
	CollectedUnix     int64  `json:"collected_unix"`
}

// restoreSample is one iteration of one restore matrix cell.
type restoreSample struct {
	Iter                int     `json:"iter"`
	TAPIMs              float64 `json:"t_api_ms"`
	TInteractiveMs      float64 `json:"t_interactive_ms"`
	SeqOK               bool    `json:"seq_ok"`
	Seq                 int     `json:"seq"`
	FaultsServed        int64   `json:"faults_served"`
	BytesCopiedFault    int64   `json:"bytes_copied_fault"`
	BytesCopiedPrefetch int64   `json:"bytes_copied_prefetch"`
}

// restoreCell mirrors one restore-*.json file (one matrix cell).
type restoreCell struct {
	Mode      string          `json:"mode"`
	MemGB     float64         `json:"mem_gb"`
	Cache     string          `json:"cache"`
	FCVersion string          `json:"fc_version"`
	DirtyMB   int             `json:"dirty_mb"`
	Skipped   *string         `json:"skipped"`
	Samples   []restoreSample `json:"samples"`
}

// isSkipped reports whether the cell was deliberately not run.
func (c restoreCell) isSkipped() bool {
	return c.Skipped != nil && *c.Skipped != ""
}

// uffdioCell is one op x threads x chunk cell of the UFFDIO_COPY sweep.
type uffdioCell struct {
	Op         string  `json:"op"`
	Threads    int     `json:"threads"`
	ChunkBytes int64   `json:"chunk_bytes"`
	TotalMB    float64 `json:"total_mb"`
	MBPerS     float64 `json:"mb_per_s"`
}

// uffdioReport mirrors uffdio-copy.json.
type uffdioReport struct {
	PageSize int          `json:"page_size"`
	RegionMB int          `json:"region_mb"`
	Seconds  int          `json:"seconds"`
	Cells    []uffdioCell `json:"cells"`
}

// zfsCell is one backend x workload fio result.
type zfsCell struct {
	Backend      string  `json:"backend"`
	Recordsize   string  `json:"recordsize"`
	Volblocksize string  `json:"volblocksize"`
	Primarycache string  `json:"primarycache"`
	Workload     string  `json:"workload"`
	IOPS         float64 `json:"iops"`
	BWMBs        float64 `json:"bw_mb_s"`
}

// zfsSyncTest is the sync=standard vs sync=disabled comparison.
type zfsSyncTest struct {
	Workload     string  `json:"workload"`
	StandardIOPS float64 `json:"standard_iops"`
	DisabledIOPS float64 `json:"disabled_iops"`
	SpeedupPct   float64 `json:"speedup_pct"`
}

// zfsReport mirrors zfs-compare.json.
type zfsReport struct {
	Caveat   string       `json:"caveat"`
	IOEngine string       `json:"ioengine"`
	Cells    []zfsCell    `json:"cells"`
	SyncTest *zfsSyncTest `json:"sync_test"`
}

func main() {
	resultsDir := flag.String("results-dir", "results", "directory containing benchmark result JSON files")
	out := flag.String("out", "results/REPORT.md", "output path for the generated markdown report")
	flag.Parse()

	report := buildReport(*resultsDir)

	if dir := filepath.Dir(*out); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fatalf("create output directory: %v", err)
		}
	}
	if err := os.WriteFile(*out, []byte(report), 0o644); err != nil {
		fatalf("write report: %v", err)
	}
	fmt.Printf("genreport: wrote %s\n", *out)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "genreport: "+format+"\n", args...)
	os.Exit(1)
}

// buildReport assembles the full markdown report from whatever result files
// are present under dir.
func buildReport(dir string) string {
	env := loadEnv(filepath.Join(dir, "env.json"))
	cells, warnings := loadRestoreCells(dir)
	uffd := loadUffdio(filepath.Join(dir, "uffdio-copy.json"))
	zfs := loadZFS(filepath.Join(dir, "zfs-compare.json"))

	// Unknown environment is treated as non-bare-metal: the disclaimer is
	// the safe default (docs/zh/06 §2).
	nested := env == nil || !isBareMetal(env.EnvType)

	var b strings.Builder
	writeHeader(&b, env, nested)
	writeRestoreSection(&b, cells)
	writeBudgetSection(&b, cells, nested)
	writeUffdioSection(&b, uffd)
	writeZFSSection(&b, zfs)
	writeMethodology(&b, cells, warnings)
	return b.String()
}

// ---------------------------------------------------------------------------
// Loading (every loader degrades to "no data" instead of failing)
// ---------------------------------------------------------------------------

func loadEnv(path string) *envInfo {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var e envInfo
	if err := json.Unmarshal(data, &e); err != nil {
		return nil
	}
	return &e
}

func loadRestoreCells(dir string) (cells []restoreCell, warnings []string) {
	matches, err := filepath.Glob(filepath.Join(dir, "restore-*.json"))
	if err != nil {
		return nil, nil
	}
	sort.Strings(matches)
	for _, path := range matches {
		data, err := os.ReadFile(path)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: unreadable, ignored (%v)", filepath.Base(path), err))
			continue
		}
		var c restoreCell
		if err := json.Unmarshal(data, &c); err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: unparsable, ignored (%v)", filepath.Base(path), err))
			continue
		}
		cells = append(cells, c)
	}
	return cells, warnings
}

func loadUffdio(path string) *uffdioReport {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var r uffdioReport
	if err := json.Unmarshal(data, &r); err != nil {
		return nil
	}
	return &r
}

func loadZFS(path string) *zfsReport {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var r zfsReport
	if err := json.Unmarshal(data, &r); err != nil {
		return nil
	}
	return &r
}

func isBareMetal(envType string) bool {
	t := strings.ToLower(strings.TrimSpace(envType))
	return strings.HasPrefix(t, "bare")
}

// ---------------------------------------------------------------------------
// Header
// ---------------------------------------------------------------------------

func writeHeader(b *strings.Builder, env *envInfo, nested bool) {
	b.WriteString("# EmberVM M0 Baseline Report\n\n")
	fmt.Fprintf(b, "Generated: %s\n\n", time.Now().UTC().Format(time.RFC3339))

	if env == nil {
		b.WriteString("Environment: " + noData + "\n\n")
	} else {
		fmt.Fprintf(b, "Environment: (%s, %s, kernel %s)\n\n", env.EnvType, env.CPUModel, env.Kernel)
		fmt.Fprintf(b, "- CPUs: %d, memory: %d MB, free disk: %d GB\n", env.CPUs, env.MemMB, env.DiskFreeGB)
		fmt.Fprintf(b, "- KVM: %t, VMX: %t, EPT: %s, unrestricted_guest: %s, cgroup v2: %t\n",
			env.KVM, env.VMX, orDash(env.EPT), orDash(env.UnrestrictedGuest), env.Cgroup2)
		if env.CollectedUnix > 0 {
			fmt.Fprintf(b, "- Environment probed at: %s\n", time.Unix(env.CollectedUnix, 0).UTC().Format(time.RFC3339))
		}
		b.WriteString("\n")
	}

	if nested {
		b.WriteString("> **DISCLAIMER: this report was produced in a nested/CI (or unknown) " +
			"environment. Nested-environment data is functional reference only; absolute " +
			"numbers MUST be re-measured on bare metal before being cited in the README " +
			"(per docs/zh/06 §2: every published benchmark carries its environment triple, " +
			"and README performance claims cite bare-metal data only).**\n\n")
	}
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// ---------------------------------------------------------------------------
// §1 Restore latency matrix
// ---------------------------------------------------------------------------

var restoreHeaders = []string{
	"mode", "mem_gb", "N",
	"t_api P50 (ms)", "t_api P90 (ms)", "t_api P99 (ms)",
	"t_int P50 (ms)", "t_int P90 (ms)", "t_int P99 (ms)",
	"seq_ok rate", "mean faults_served",
}

func writeRestoreSection(b *strings.Builder, cells []restoreCell) {
	b.WriteString("## 1. Restore latency matrix\n\n")
	if len(cells) == 0 {
		b.WriteString(noData + "\n\n")
		return
	}

	if versions := distinctFCVersions(cells); len(versions) > 0 {
		fmt.Fprintf(b, "Firecracker version(s): %s.\n\n", strings.Join(versions, ", "))
	}

	groups := map[string][]restoreCell{}
	for _, c := range cells {
		groups[c.Cache] = append(groups[c.Cache], c)
	}

	for _, cache := range cacheOrder(groups) {
		group := groups[cache]
		sort.SliceStable(group, func(i, j int) bool {
			if group[i].MemGB != group[j].MemGB {
				return group[i].MemGB < group[j].MemGB
			}
			return group[i].Mode < group[j].Mode
		})

		fmt.Fprintf(b, "### %s cache\n\n", orDash(cache))
		rows := make([][]string, 0, len(group))
		for _, c := range group {
			rows = append(rows, restoreRow(c))
		}
		b.WriteString(benchstat.MarkdownTable(restoreHeaders, rows))
		b.WriteString("\n")
	}
}

func restoreRow(c restoreCell) []string {
	mem := formatMemGB(c.MemGB)
	if c.isSkipped() {
		return []string{
			c.Mode, mem, "—",
			fmt.Sprintf("_skipped: %s_", *c.Skipped), "—", "—",
			"—", "—", "—",
			"—", "—",
		}
	}
	if len(c.Samples) == 0 {
		return []string{
			c.Mode, mem, "0",
			"_no samples_", "—", "—",
			"—", "—", "—",
			"—", "—",
		}
	}

	api := make([]float64, 0, len(c.Samples))
	inter := make([]float64, 0, len(c.Samples))
	seqOK := 0
	var faultsSum float64
	for _, s := range c.Samples {
		api = append(api, s.TAPIMs)
		inter = append(inter, s.TInteractiveMs)
		if s.SeqOK {
			seqOK++
		}
		faultsSum += float64(s.FaultsServed)
	}
	apiSum := benchstat.Summarize(api)
	intSum := benchstat.Summarize(inter)
	n := len(c.Samples)

	return []string{
		c.Mode, mem, fmt.Sprintf("%d", n),
		f1(apiSum.P50), f1(apiSum.P90), f1(apiSum.P99),
		f1(intSum.P50), f1(intSum.P90), f1(intSum.P99),
		fmt.Sprintf("%.1f%%", 100*float64(seqOK)/float64(n)),
		f1(faultsSum / float64(n)),
	}
}

func distinctFCVersions(cells []restoreCell) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range cells {
		if c.FCVersion != "" && !seen[c.FCVersion] {
			seen[c.FCVersion] = true
			out = append(out, c.FCVersion)
		}
	}
	sort.Strings(out)
	return out
}

func cacheOrder(groups map[string][]restoreCell) []string {
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	rank := func(c string) int {
		switch c {
		case "cold":
			return 0
		case "warm":
			return 1
		default:
			return 2
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		ri, rj := rank(keys[i]), rank(keys[j])
		if ri != rj {
			return ri < rj
		}
		return keys[i] < keys[j]
	})
	return keys
}

// ---------------------------------------------------------------------------
// §2 Budget-model comparison (docs/zh/04 §2)
// ---------------------------------------------------------------------------

func writeBudgetSection(b *strings.Builder, cells []restoreCell, nested bool) {
	b.WriteString("## 2. Budget-model comparison (docs/zh/04 §2)\n\n")
	b.WriteString("Hot-restore path (warm host page cache) measured medians against the " +
		"documented end-to-end latency budget model.\n\n")

	// Row 1: snapfile load + device rebuild vs t_api_ms P50. The budget line
	// covers only API-visible restore work, so all modes/tiers are pooled.
	apiSamples, apiNote := pooledWithFallback(cells, "warm", "",
		func(s restoreSample) float64 { return s.TAPIMs })

	// Row 2: interactive budget vs uffd-prefetch t_interactive_ms P50 (the
	// production restore mode: WS prefetch + background fill, docs/zh/04 #2).
	intSamples, intNote := pooledWithFallback(cells, "warm", "uffd-prefetch",
		func(s restoreSample) float64 { return s.TInteractiveMs })

	headers := []string{"budget line", "documented budget", "measured P50 (ms)", "Δ vs budget max", "verdict"}
	rows := [][]string{
		budgetRow("snapfile load + device rebuild (t_api_ms)", "5-20 ms", budgetAPIMaxMs, apiSamples, nested),
		budgetRow("interactive, hot restore, uffd-prefetch (t_interactive_ms)", "P50 < 500 ms", budgetInteractiveMaxMs, intSamples, nested),
	}
	b.WriteString(benchstat.MarkdownTable(headers, rows))
	b.WriteString("\n")

	fmt.Fprintf(b, "- t_api_ms pool: %s\n", apiNote)
	fmt.Fprintf(b, "- t_interactive_ms pool: %s\n\n", intNote)
}

func budgetRow(name, documented string, budgetMax float64, samples []float64, nested bool) []string {
	if len(samples) == 0 {
		return []string{name, documented, noData, "—", "—"}
	}
	p50 := benchstat.Summarize(samples).P50
	verdict := "within budget"
	if p50 > budgetMax {
		verdict = "over budget"
	}
	if nested {
		verdict += " — CI reference only"
	}
	return []string{name, documented, f1(p50), fmt.Sprintf("%+.1f ms", p50-budgetMax), verdict}
}

// pooledWithFallback pools samples from cells matching cache/mode ("" means
// any). If the preferred cache has no data it falls back to all cache states
// and says so in the returned note.
func pooledWithFallback(cells []restoreCell, cache, mode string, pick func(restoreSample) float64) ([]float64, string) {
	samples := poolSamples(cells, cache, mode, pick)
	scope := "all modes and memory tiers"
	if mode != "" {
		scope = fmt.Sprintf("mode=%s, all memory tiers", mode)
	}
	if len(samples) > 0 {
		return samples, fmt.Sprintf("%s cache, %s (%d samples)", cache, scope, len(samples))
	}
	samples = poolSamples(cells, "", mode, pick)
	if len(samples) > 0 {
		return samples, fmt.Sprintf("no %s-cache data — all cache states pooled, %s (%d samples)", cache, scope, len(samples))
	}
	return nil, "no data"
}

func poolSamples(cells []restoreCell, cache, mode string, pick func(restoreSample) float64) []float64 {
	var out []float64
	for _, c := range cells {
		if c.isSkipped() {
			continue
		}
		if cache != "" && c.Cache != cache {
			continue
		}
		if mode != "" && c.Mode != mode {
			continue
		}
		for _, s := range c.Samples {
			out = append(out, pick(s))
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// §3 UFFDIO_COPY throughput
// ---------------------------------------------------------------------------

func writeUffdioSection(b *strings.Builder, r *uffdioReport) {
	b.WriteString("## 3. UFFDIO_COPY throughput\n\n")
	if r == nil || len(r.Cells) == 0 {
		b.WriteString(noData + "\n\n")
		return
	}

	fmt.Fprintf(b, "page_size = %d B, region = %d MB, duration = %d s per cell.\n\n",
		r.PageSize, r.RegionMB, r.Seconds)

	cells := make([]uffdioCell, len(r.Cells))
	copy(cells, r.Cells)
	sort.Slice(cells, func(i, j int) bool {
		if cells[i].Op != cells[j].Op {
			return cells[i].Op < cells[j].Op
		}
		if cells[i].Threads != cells[j].Threads {
			return cells[i].Threads < cells[j].Threads
		}
		return cells[i].ChunkBytes < cells[j].ChunkBytes
	})

	headers := []string{"op", "threads", "chunk (bytes)", "total copied (MB)", "MB/s"}
	rows := make([][]string, 0, len(cells))
	best := cells[0]
	for _, c := range cells {
		rows = append(rows, []string{
			c.Op,
			fmt.Sprintf("%d", c.Threads),
			fmt.Sprintf("%d", c.ChunkBytes),
			f1(c.TotalMB),
			f1(c.MBPerS),
		})
		if c.MBPerS > best.MBPerS {
			best = c
		}
	}
	b.WriteString(benchstat.MarkdownTable(headers, rows))
	b.WriteString("\n")

	position := "below"
	switch {
	case best.MBPerS > reapBandHighMBs:
		position = "above"
	case best.MBPerS >= reapBandLowMBs:
		position = "within"
	}
	fmt.Fprintf(b, "Reference: REAP's published uffd delivery band is %.1f-%.1f MB/s "+
		"(24.6 µs/page fault-path floor to eager sequential working-set read). "+
		"Best measured cell — %s, %d thread(s), %d B chunks — reached %.1f MB/s, %s that band. "+
		"Cells stuck near the band's floor indicate a page-at-a-time fault path; the prefetch "+
		"path must batch UFFDIO_COPY in large chunks to stay near the top of the band.\n\n",
		reapBandLowMBs, reapBandHighMBs, best.Op, best.Threads, best.ChunkBytes, best.MBPerS, position)
}

// ---------------------------------------------------------------------------
// §4 ZFS raw-file-on-dataset vs zvol
// ---------------------------------------------------------------------------

func writeZFSSection(b *strings.Builder, r *zfsReport) {
	b.WriteString("## 4. ZFS raw-file-on-dataset vs zvol\n\n")
	if r == nil || len(r.Cells) == 0 {
		b.WriteString(noData + "\n\n")
		return
	}

	if r.Caveat != "" {
		fmt.Fprintf(b, "> **Caveat: %s**\n\n", r.Caveat)
	}
	if r.IOEngine != "" {
		fmt.Fprintf(b, "fio ioengine: %s.\n\n", r.IOEngine)
	}

	cells := make([]zfsCell, len(r.Cells))
	copy(cells, r.Cells)
	sort.SliceStable(cells, func(i, j int) bool {
		if cells[i].Workload != cells[j].Workload {
			return cells[i].Workload < cells[j].Workload
		}
		return cells[i].Backend < cells[j].Backend
	})

	headers := []string{"backend", "block size", "primarycache", "workload", "IOPS", "BW (MB/s)"}
	rows := make([][]string, 0, len(cells))
	for _, c := range cells {
		bs := c.Recordsize
		if bs == "" {
			bs = c.Volblocksize
		}
		rows = append(rows, []string{
			c.Backend, orDash(bs), orDash(c.Primarycache), c.Workload,
			f1(c.IOPS), f1(c.BWMBs),
		})
	}
	b.WriteString(benchstat.MarkdownTable(headers, rows))
	b.WriteString("\n")

	writeZFSRatios(b, cells)

	if st := r.SyncTest; st != nil && st.Workload != "" {
		speedup := st.SpeedupPct
		if speedup == 0 && st.StandardIOPS > 0 {
			speedup = (st.DisabledIOPS/st.StandardIOPS - 1) * 100
		}
		fmt.Fprintf(b, "sync=disabled vs sync=standard (%s): %.1f → %.1f IOPS (+%.1f%%). "+
			"sync=disabled trades crash consistency for latency and is only acceptable for "+
			"reconstructible scratch data.\n\n",
			st.Workload, st.StandardIOPS, st.DisabledIOPS, speedup)
	}
}

// writeZFSRatios prints the raw-file/zvol IOPS ratio per workload, using the
// best (highest-IOPS) cell per backend when several block sizes were tested.
func writeZFSRatios(b *strings.Builder, cells []zfsCell) {
	best := map[string]map[string]float64{} // workload -> backend -> best IOPS
	for _, c := range cells {
		if best[c.Workload] == nil {
			best[c.Workload] = map[string]float64{}
		}
		if c.IOPS > best[c.Workload][c.Backend] {
			best[c.Workload][c.Backend] = c.IOPS
		}
	}

	workloads := make([]string, 0, len(best))
	for w := range best {
		workloads = append(workloads, w)
	}
	sort.Strings(workloads)

	var lines []string
	for _, w := range workloads {
		raw, hasRaw := best[w]["dataset-rawfile"]
		zvol, hasZvol := best[w]["zvol"]
		if !hasRaw || !hasZvol {
			continue
		}
		if zvol == 0 {
			lines = append(lines, fmt.Sprintf("- %s: raw-file/zvol IOPS ratio = n/a (zvol IOPS is 0)", w))
			continue
		}
		lines = append(lines, fmt.Sprintf("- %s: raw-file/zvol IOPS ratio = %.1fx", w, raw/zvol))
	}
	if len(lines) == 0 {
		return
	}
	b.WriteString("Raw-file-on-dataset vs zvol (best cell per backend):\n\n")
	b.WriteString(strings.Join(lines, "\n"))
	b.WriteString("\n\n")
}

// ---------------------------------------------------------------------------
// §5 Methodology appendix
// ---------------------------------------------------------------------------

func writeMethodology(b *strings.Builder, cells []restoreCell, warnings []string) {
	b.WriteString("## 5. Methodology appendix\n\n")

	b.WriteString("### Timing points\n\n")
	b.WriteString("- T0: host monotonic clock read immediately before issuing the `PUT /snapshot/load` API request.\n")
	b.WriteString("- T1: API `204 No Content` response received.\n")
	b.WriteString("- T2: first successful guest TCP probe round-trip completes.\n")
	b.WriteString("- `t_api_ms = T1 - T0`; `t_interactive_ms = T2 - T0`.\n\n")

	b.WriteString("### Procedure\n\n")
	b.WriteString("- Iteration counts per matrix cell are reported in the `N` column of the §1 tables.\n")
	b.WriteString("- Cold cache: `echo 3 > /proc/sys/vm/drop_caches` is executed on the host before every cold-cache iteration.\n")
	b.WriteString("- Isolation: every iteration launches a fresh Firecracker process inside a fresh network namespace; no process or netns is reused across iterations.\n\n")

	b.WriteString("### Skip/downgrade log\n\n")
	var entries []string
	for _, c := range cells {
		if c.isSkipped() {
			entries = append(entries, fmt.Sprintf("- skipped cell %s / %s / %s GB: %s",
				orDash(c.Cache), c.Mode, formatMemGB(c.MemGB), *c.Skipped))
		}
	}
	for _, w := range warnings {
		entries = append(entries, "- "+w)
	}
	if len(entries) == 0 {
		b.WriteString("- none\n")
	} else {
		b.WriteString(strings.Join(entries, "\n"))
		b.WriteString("\n")
	}
}

// ---------------------------------------------------------------------------
// Formatting helpers
// ---------------------------------------------------------------------------

func f1(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "—"
	}
	return fmt.Sprintf("%.1f", v)
}

// formatMemGB renders integer memory tiers without a trailing ".0".
func formatMemGB(v float64) string {
	if v == math.Trunc(v) && !math.IsInf(v, 0) {
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprintf("%.1f", v)
}
