# Benchmark guide: extended label processing

This document describes the benchmarks for `moduleConfig.manageExtendedLabels`, their fixtures, how to run them, and the measured results through October 2026.

## Scope and measurement methodology

`manageExtendedLabels` binds configured rules to a metric family, enriches individual metrics with labels, and serializes the result as Prometheus text using `expfmt.MetricFamilyToText`. Benchmarks therefore measure **enrichment plus serialization**, not just lookup cost.

Each fixture represents **one metric family**, not a complete exporter scrape. `metrics-1000` means 1,000 samples in that family, not 1,000 metric names. HTTP proxying, decoding, compression, target selection across the fleet, and whole-fleet configuration loading are outside these measurements.

The multiple-rule and production benchmarks compile their configurations before timing and clone each input metric family outside the timer to avoid accumulating appended labels between iterations. The cloning itself is excluded from measured `ns/op` and `B/op`, but can still influence runtime garbage collection. **Caveat:** the older `BenchmarkManageExtendedLabels` calls `compileTargetForTest` inside its timed loop; its results are therefore not strictly interchangeable with those of the other two benchmarks.

All recorded runs were on Linux/amd64, Intel Core i7-13800H, with `-benchtime=2s -count=15`. Values presented below are medians or benchstat comparisons, not individual observations.

## Benchmark fixtures

### `BenchmarkManageExtendedLabels` — original baseline

Cases: `metrics-100-match-keys-10`, `metrics-1000-match-keys-10`.

One `MatchLabel` rule on `ifDescr`, ten exact interface values and a wildcard (`*`). The metric family is `ifAdminStatus`. The older benchmark's in-loop configuration compilation should be considered when interpreting its results.

### `BenchmarkManageExtendedLabelsMultipleRules` — multi-rule stress test

Cases: 100 or 1,000 metrics, with 1, 2, 5, or 10 rules.

Each rule has ten exact interface values plus a wildcard. Every rule matches the metric name and metric-label value. Each matching rule adds four labels: two exact-match labels, one wildcard label, and one default label. Generated label names are distinct across rules.

This is intentionally a high-output stress case: increasing the number of rules also increases the number of serialized labels. `match-keys-10` counts map entries within one rule; `rules-10` counts independent matching rule objects.

### `BenchmarkManageExtendedLabelsProduction` — production-representative single-rule workload

Cases: 100 or 1,000 metrics, each at 25%, 50%, or 100% configured-interface matches.

The fixture models a **single device** with one `ifDescr` rule and 36 exact interface mappings. Each matched interface receives three labels: `discovery`, `neighbor`, and `remote_interface`. Discovery varies by interface (CDP and LLDP); it is **not** a device-wide constant. Unmatched interfaces receive no enrichment. There is no wildcard and no default label.

The benchmark intentionally exercises the **single-rule sequential fast path**, which is the common production configuration. A deployment with approximately 4,000 devices does not imply that every scrape checks all 4,000 device configurations: each operation here uses one selected target. Fleet-wide configuration compilation and memory consumption would require a separate benchmark.

The 25%, 50%, and 100% match rates are controlled scenarios, not measured production distributions.

## Implementation history

1. **Per-family metric-name matching:** avoid repeating metric-name regex evaluation for every sample.
2. **Precompiled reusable label pairs:** compile configuration-derived protobuf label pairs once, then reuse them during scrapes.
3. **Batched multi-rule appends:** when several rules match, collect label-pair slices in reusable scratch storage, reserve capacity for the final label slice once, and append the collected labels.
4. **Single-rule fast path:** use the simpler sequential path for zero or one bound matching rule, avoiding unnecessary batching overhead for typical production configurations.

The sequential and batched implementations are compared by a differential correctness test. This verifies equivalent output for its test fixtures, not an independent proof of correctness for every possible input shape.

## Running benchmarks

```bash
# Original benchmark (note the in-loop compilation caveat)
go test -run='^$' -bench='^BenchmarkManageExtendedLabels$' -benchmem -benchtime=2s -count=15 .

# Multiple matching rules
go test -run='^$' -bench='^BenchmarkManageExtendedLabelsMultipleRules$' -benchmem -benchtime=2s -count=15 . | tee multirules.txt

# Production-representative workload
go test -run='^$' -bench='^BenchmarkManageExtendedLabelsProduction$' -benchmem -benchtime=2s -count=15 . | tee production-after3.txt

# Compare equivalent benchmark runs on different revisions
benchstat production-before3.txt production-after3.txt

# Tests and race detection
go test ./...
go test -race ./...
```

To benchmark a committed baseline without disturbing local changes, create a separate worktree at the relevant revision. If the baseline revision predates the new benchmark, copy **only the benchmark and its fixture** into that worktree, ensuring they compile against the older implementation; do not copy the current implementation. Run with the same Go version, hardware, settings and fixture. Save output outside the temporary worktree before removing it.

For CPU profiling:

```bash
go test -run='^$' -bench='^BenchmarkManageExtendedLabels$' -cpuprofile=cpu.out .
go tool pprof -http=127.0.0.1:8080 cpu.out
```

Fixture preparation excluded from the benchmark timer may nevertheless show up in a CPU profile.

### Reading the output

For a line such as:

```text
BenchmarkManageExtendedLabelsMultipleRules/metrics-1000-rules-2-20  1628  1337806 ns/op  654497 B/op  2020 allocs/op
```

`1628` is the iteration count, `ns/op` is time per measured operation, `B/op` is allocated bytes per operation, and `allocs/op` is heap allocations per operation. The trailing `-20` denotes `GOMAXPROCS`. Smaller numbers are generally preferable **for equivalent workloads**.

## Historical results: multiple rules before optimization #3

These figures were recorded **before batching was introduced**. They should not be mistaken for the latest implementation's performance.

| Metrics | Rules | Median time/op | B/op (approx.) | Allocs/op |
|---:|---:|---:|---:|---:|
| 100 | 1 | 64.7 µs | 37,985 | 114 |
| 100 | 2 | 122.6 µs | 78,874 | 215 |
| 100 | 5 | 275.4 µs | 187,991 | 516 |
| 100 | 10 | 593.7 µs | 460,693 | 1,018 |
| 1,000 | 1 | 681.7 µs | 311,565 | 1,018 |
| 1,000 | 2 | 1,296.7 µs | 654,494 | 2,020 |
| 1,000 | 5 | 2,970.6 µs | 1,613,532 | 5,026 |
| 1,000 | 10 | 6,860.3 µs | 4,073,097 | 10,031 |

Time and output grow with the number of matching rules. Allocation growth is consistent with repeated label-slice growth, but the benchmark by itself does not prove the source of every allocation.

## Results: multiple rules after batching and fast path

These are results from a subsequent multi-rule benchmark run with the latest implementation. Values are approximate medians from the recorded runs; unlike the production comparison below, this is **not** a fresh same-fixture `benchstat` comparison against the baseline.

| Metrics | Rules | Median time/op | Approx. memory/op | Allocs/op |
|---:|---:|---:|---:|---:|
| 100 | 1 | 75.38 µs | 37.09 KiB | 114 |
| 100 | 2 | 124.0 µs | 72.68 KiB | 119 |
| 100 | 5 | 233.5 µs | 146.7 KiB | 121 |
| 100 | 10 | 449.0 µs | 293.4 KiB | 123 |
| 1,000 | 1 | 733.7 µs | 304.3 KiB | 1,018 |
| 1,000 | 2 | 1.277 ms | 592.6 KiB | 1,024 |
| 1,000 | 5 | 2.625 ms | 1.172 MiB | 1,030 |
| 1,000 | 10 | 4.378 ms | 2.343 MiB | 1,032 |

The clearest benefit is at high rule counts: for 1,000 metrics and 10 rules, the approximate median decreased from 6.860 ms to 4.378 ms, and allocations fell from roughly 10,031 to 1,032. The extra serialized output still makes this an end-to-end enrichment-and-serialization benchmark, not an isolated measure of batching.

The single-rule cases above come from different runs and should not be used to claim a single-rule regression. The controlled production comparison below is the stronger evidence for that path.

## Results: production benchmark, before vs. after optimization #3

The same production fixture was run against the last committed implementation (`production-before3.txt`) and the newer implementation with batching and the single-rule fast path (`production-after3.txt`). Both runs used 15 samples per case.

| Metrics | Match rate | Before median | After median | Change | Statistical result |
|---:|---:|---:|---:|---:|---|
| 100 | 25% | 44.70 µs | 41.85 µs | -6.39% | p=0.001 |
| 100 | 50% | 57.59 µs | 55.94 µs | Not significant | p=0.202 |
| 100 | 100% | 69.42 µs | 68.71 µs | Not significant | p=0.935 |
| 1,000 | 25% | 544.1 µs | 524.9 µs | -3.53% | p=0.041 |
| 1,000 | 50% | 613.3 µs | 574.1 µs | -6.40% | p=0.003 |
| 1,000 | 100% | 907.5 µs | 847.7 µs | -6.59% | p=0.016 |

The geometric mean of benchmark times decreased from **194.5 µs to 185.8 µs (-4.49%)**. Four of six cases showed statistically significant improvements at p < 0.05; no case showed a statistically significant regression.

### Allocated bytes and allocation counts

Allocated bytes were effectively unchanged (geometric mean **96.70 KiB/op** for both revisions). A few near-zero differences in `B/op` had small p-values, but they have no practical significance at the displayed precision.

| Metrics | Match rate | Allocs/op, before | Allocs/op, after |
|---:|---:|---:|---:|
| 100 | 25% | 38 | 38 |
| 100 | 50% | 64 | 64 |
| 100 | 100% | 114 | 114 |
| 1,000 | 25% | 268 | 268 |
| 1,000 | 50% | 518 | 518 |
| 1,000 | 100% | ~1,020 | ~1,020 |

The stable allocation counts support the intended behavior of the single-rule fast path. The modest time improvements may include compiler, code-layout, or benchmark-environment effects and should not be attributed entirely to algorithmic changes.

### Validation

The following commands passed after the implementation changes:

```bash
go test ./...
go test -race ./...
```

The test suite includes a differential comparison of sequential and batched output. The production benchmark found no meaningful regression against the prior committed implementation.

## Configuration correspondence

A production-style mapping assigns discovery metadata **per interface**, for example:

```yaml
targets:
  device-a:
    labels:
      - label_key_name: ifDescr
        extended_labels:
          - metric_match: '^ifAdminStatus$'
            match_label_key_values:
              TwentyFiveGigE2/0/1:
                label_pairs:
                  discovery: CDP
                  neighbor: switch1.example.net
                  remote_interface: TenGigabitEthernet1/1/3
              TwentyFiveGigE2/0/2:
                label_pairs:
                  discovery: LLDP
                  neighbor: switch2.example.net
                  remote_interface: Ethernet1/1
```

This is **one matching rule with multiple exact-value entries**, not one rule per interface. The production benchmark models 36 entries of this kind.

By contrast, the multiple-rule fixture models several entries under `extended_labels`, each matching the same metric and contributing its own labels. It additionally uses wildcard and default labels to stress repeated appending.

## Remaining limitations and potential future work

- **Mixed metric label shapes:** the per-family label-index binding assumes a stable label position across samples. If the order or shape varies, index-based lookup may target the wrong label. This is a separate correctness consideration and is not resolved by these performance benchmarks.
- **Whole-scrape profiling:** these numbers cover one metric family, not the end-to-end HTTP scrape path.
- **Fleet configuration scale:** a separate startup benchmark could measure parsing, compiling, and retaining mappings for approximately 4,000 devices; it is not necessary to validate the per-scrape optimization.
- **One-of-many rule matches:** a future fixture could hold the number of appended labels constant while increasing the number of configured rules, helping isolate rule-selection overhead from serialization.

## Conclusion

The production-representative benchmark shows **no performance regression** from optimization #3 and its single-rule fast path: execution time improved by 4.49% in geometric mean, with essentially unchanged memory and allocation counts. Multi-rule stress measurements show substantial reductions in allocation count and better throughput at high rule counts. The current implementation is suitable to retain; additional optimization should be driven by production profiles or new requirements rather than further synthetic micro-optimizations.