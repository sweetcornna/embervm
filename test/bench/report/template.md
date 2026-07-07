<!--
  This file is DOCUMENTATION ONLY: a human-readable specimen of the report
  that test/bench/report/genreport.go emits. Do not edit REPORT.md by hand —
  it is generated:

      go run ./test/bench/report --results-dir results --out results/REPORT.md

  All placeholder values below (12.3, v1.16.1, ...) are illustrative. Section
  numbering and ordering are exactly what the generator produces. Any section
  whose input file is missing or unparsable is rendered as
  "_no data collected in this run_" instead of its tables.
-->

# EmberVM M0 Baseline Report

Generated: 2026-07-07T10:00:00Z

Environment: (nested@github-actions, AMD EPYC 7763 64-Core Processor, kernel 6.8.0-1021-azure)

- CPUs: 4, memory: 16000 MB, free disk: 40 GB
- KVM: true, VMX: true, EPT: Y, unrestricted_guest: Y, cgroup v2: true
- Environment probed at: 2026-07-07T09:55:00Z

> **DISCLAIMER: this report was produced in a nested/CI (or unknown) environment. Nested-environment data is functional reference only; absolute numbers MUST be re-measured on bare metal before being cited in the README (per docs/zh/06 §2: every published benchmark carries its environment triple, and README performance claims cite bare-metal data only).**

<!-- The disclaimer block is omitted when env_type starts with "bare"
     (bare-metal). A missing/unparsable env.json renders
     "Environment: _no data collected in this run_" and keeps the disclaimer,
     since an unknown environment must not be mistaken for bare metal. -->

## 1. Restore latency matrix

Firecracker version(s): v1.16.1.

<!-- One table per cache group, "cold" first, then "warm", then any other
     cache values alphabetically. Rows sorted by mem_gb, then mode.
     Input: results/restore-*.json (one file per matrix cell). -->

### cold cache

| mode | mem_gb | N | t_api P50 (ms) | t_api P90 (ms) | t_api P99 (ms) | t_int P50 (ms) | t_int P90 (ms) | t_int P99 (ms) | seq_ok rate | mean faults_served |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| file | 2 | 20 | 14.2 | 18.8 | 19.8 | 410.5 | 506.3 | 527.8 | 100.0% | 0.0 |
| uffd-lazy | 2 | 20 | 12.9 | 15.1 | 16.0 | 1250.7 | 1502.3 | 1611.9 | 100.0% | 8412.5 |
| uffd-prefetch | 2 | 20 | 13.1 | 15.4 | 16.2 | 480.2 | 560.8 | 590.1 | 100.0% | 812.3 |
| file | 4 | 20 | 15.0 | 19.2 | 20.5 | 640.1 | 720.6 | 755.2 | 100.0% | 0.0 |
| uffd-lazy | 16 | — | _skipped: 16GB tier does not fit on a 16GB runner_ | — | — | — | — | — | — | — |

### warm cache

| mode | mem_gb | N | t_api P50 (ms) | t_api P90 (ms) | t_api P99 (ms) | t_int P50 (ms) | t_int P90 (ms) | t_int P99 (ms) | seq_ok rate | mean faults_served |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| file | 2 | 20 | 10.2 | 11.8 | 12.4 | 205.5 | 240.3 | 260.8 | 100.0% | 0.0 |
| uffd-prefetch | 2 | 20 | 10.8 | 11.9 | 12.6 | 209.7 | 218.2 | 220.2 | 100.0% | 801.0 |

## 2. Budget-model comparison (docs/zh/04 §2)

Hot-restore path (warm host page cache) measured medians against the documented end-to-end latency budget model.

<!-- Budget lines come from docs/zh/04 §2. Measured P50s are pooled from the
     §1 warm-cache data (falling back to all cache states when no warm data
     exists — the pool notes below the table say which was used). The verdict
     carries "— CI reference only" whenever the environment is not bare
     metal. -->

| budget line | documented budget | measured P50 (ms) | Δ vs budget max | verdict |
| --- | --- | --- | --- | --- |
| snapfile load + device rebuild (t_api_ms) | 5-20 ms | 10.8 | -9.2 ms | within budget — CI reference only |
| interactive, hot restore, uffd-prefetch (t_interactive_ms) | P50 < 500 ms | 209.7 | -290.4 ms | within budget — CI reference only |

- t_api_ms pool: warm cache, all modes and memory tiers (40 samples)
- t_interactive_ms pool: warm cache, mode=uffd-prefetch, all memory tiers (20 samples)

## 3. UFFDIO_COPY throughput

page_size = 4096 B, region = 512 MB, duration = 3 s per cell.

<!-- Input: results/uffdio-copy.json. Rows sorted by op, threads, chunk. -->

| op | threads | chunk (bytes) | total copied (MB) | MB/s |
| --- | --- | --- | --- | --- |
| copy | 1 | 4096 | 812.5 | 270.8 |
| copy | 1 | 65536 | 1450.0 | 483.3 |
| copy | 4 | 4096 | 1980.2 | 660.1 |
| copy | 4 | 65536 | 3600.0 | 1200.0 |
| zeropage | 1 | 4096 | 950.0 | 316.7 |

Reference: REAP's published uffd delivery band is 260.0-533.0 MB/s (24.6 µs/page fault-path floor to eager sequential working-set read). Best measured cell — copy, 4 thread(s), 65536 B chunks — reached 1200.0 MB/s, above that band. Cells stuck near the band's floor indicate a page-at-a-time fault path; the prefetch path must batch UFFDIO_COPY in large chunks to stay near the top of the band.

## 4. ZFS raw-file-on-dataset vs zvol

> **Caveat: loopback pool on nested VM, functional reference only**

fio ioengine: psync.

<!-- Input: results/zfs-compare.json. The caveat string from the input file
     is always rendered prominently, first. "block size" is recordsize for
     dataset-rawfile cells and volblocksize for zvol cells. -->

| backend | block size | primarycache | workload | IOPS | BW (MB/s) |
| --- | --- | --- | --- | --- | --- |
| dataset-rawfile | 16k | all | randread-4k | 123.0 | 45.6 |
| zvol | 16k | all | randread-4k | 100.0 | 39.1 |
| dataset-rawfile | 16k | all | randwrite-4k | 98.0 | 36.2 |
| zvol | 16k | all | randwrite-4k | 85.0 | 31.4 |

Raw-file-on-dataset vs zvol (best cell per backend):

- randread-4k: raw-file/zvol IOPS ratio = 1.2x
- randwrite-4k: raw-file/zvol IOPS ratio = 1.2x

sync=disabled vs sync=standard (randwrite-4k-fsync1): 100.0 → 450.0 IOPS (+350.0%). sync=disabled trades crash consistency for latency and is only acceptable for reconstructible scratch data.

## 5. Methodology appendix

### Timing points

- T0: host monotonic clock read immediately before issuing the `PUT /snapshot/load` API request.
- T1: API `204 No Content` response received.
- T2: first successful guest TCP probe round-trip completes.
- `t_api_ms = T1 - T0`; `t_interactive_ms = T2 - T0`.

### Procedure

- Iteration counts per matrix cell are reported in the `N` column of the §1 tables.
- Cold cache: `echo 3 > /proc/sys/vm/drop_caches` is executed on the host before every cold-cache iteration.
- Isolation: every iteration launches a fresh Firecracker process inside a fresh network namespace; no process or netns is reused across iterations.

### Skip/downgrade log

<!-- One entry per skipped matrix cell (with its reason string) and per
     unreadable/unparsable restore-*.json file; "- none" when empty. -->

- skipped cell cold / uffd-lazy / 16 GB: 16GB tier does not fit on a 16GB runner
