# Benchmark guide: extended label processing

This document describes the benchmarks for `moduleConfig.manageExtendedLabels`, their fixtures, how to run them, and how to interpret results.

## What is measured

Both benchmarks time a call to `manageExtendedLabels`, including metric-family rule binding, per-metric label matching and appending, and Prometheus text serialization (`expfmt.MetricFamilyToText`). They do **not** isolate the matching algorithm from output serialization.

The benchmark fixtures are constructed before timing. Each iteration clones the input metric family to avoid accumulating appended labels between iterations. Cloning is excluded from the benchmark timer with `b.StopTimer()` and `b.StartTimer()`, although it can still appear in CPU profiles and affect GC activity.

## Benchmarks and case names

Both benchmarks live in `http_test.go`.

### `BenchmarkManageExtendedLabels`

The baseline benchmark varies the number of metrics in a single metric family. Example cases:

- `metrics-100-match-keys-10`
- `metrics-1000-match-keys-10`

`metrics-100` means 100 samples in one `MetricFamily`, **not** 100 metric names. `match-keys-10` means ten exact values in `match_label_key_values`, plus a wildcard (`*`). The metric family is named `ifAdminStatus`; individual samples have different `ifDescr` values. The baseline uses one `MatchLabel` rule for its configured target label.

### `BenchmarkManageExtendedLabelsMultipleRules`

The new benchmark varies the number of `MatchLabel` rules applied to the same metric family. Example cases:

- `metrics-100-rules-1`
- `metrics-100-rules-2`
- `metrics-100-rules-5`
- `metrics-100-rules-10`
- The same four cases with `metrics-1000`.

Each rule has ten exact match values (plus a wildcard), and **all rules match** the metric name and metric-label value. Each matching rule adds four labels: two from the exact match, one from the wildcard, and one default. Distinct generated label names prevent collisions between rules.

**Match keys are not matching rules:** `match-keys-10` is the number of values in one rule's lookup table, while `rules-10` is the number of separate rule objects applied to each metric.

This is a deliberate high-output workload. Increasing the number of rules also increases the number of serialized labels; the measured time therefore includes both rule-processing cost and additional text output.

## Running benchmarks

Run the baseline:

```bash
go test -run='^$' -bench='^BenchmarkManageExtendedLabels$' -benchmem -benchtime=2s -count=15 .
```

Run the multi-rule benchmark:

```bash
go test -run='^$' -bench='^BenchmarkManageExtendedLabelsMultipleRules$' -benchmem -benchtime=2s -count=15 . | tee multirules.txt
```

Run CPU profiling (remember that setup may appear in the profile):

```bash
go test -run='^$' -bench='^BenchmarkManageExtendedLabels$' -cpuprofile=cpu.out .
go tool pprof -http=127.0.0.1:8080 cpu.out
```

For comparisons between code changes, keep the same hardware, Go version, environment, fixture, and benchmark flags. Save before/after runs and compare with `benchstat`.

## Understanding benchmark output

Example:

```text
BenchmarkManageExtendedLabelsMultipleRules/metrics-1000-rules-2-20  1628  1337806 ns/op  654497 B/op  2020 allocs/op
```

- The number after the case name (`1628`) is the iteration count (`b.N`).
- `ns/op` is time per measured call, including serialization.
- `B/op` is allocated heap bytes per measured operation.
- `allocs/op` is heap allocations per measured operation.
- The trailing `-20` indicates the run's `GOMAXPROCS` value.

Lower numbers are generally better, but benchmarks with more matching rules also generate more output and should not be treated as identical workloads.

## Results: multiple matching rules (October 2026)

Linux/amd64; Intel Core i7-13800H; 15 runs per case; `-benchtime=2s`. Values below are approximate **medians**, not single runs.

| Metrics | Rules | Median time/op | Approx. B/op | Allocs/op |
|---:|---:|---:|---:|---:|
| 100 | 1 | 64.7 µs | 37,985 | 114 |
| 100 | 2 | 122.6 µs | 78,874 | 215 |
| 100 | 5 | 275.4 µs | 187,991 | 516 |
| 100 | 10 | 593.7 µs | 460,693 | 1,018 |
| 1,000 | 1 | 681.7 µs | 311,565 | 1,018 |
| 1,000 | 2 | 1,296.7 µs | 654,494 | 2,020 |
| 1,000 | 5 | 2,970.6 µs | 1,613,532 | 5,026 |
| 1,000 | 10 | 6,860.3 µs | 4,073,097 | 10,031 |

### Interpretation

- Time grows approximately with the number of matching rules, but each additional rule also increases the serialized output.
- Allocations grow by approximately one allocation per metric per rule. This is **consistent with** repeated growth of `metric.Label` backing slices when each rule appends labels, but the benchmark alone does not prove the allocation source; use allocation profiling to verify.
- For 1,000 metrics, one rule adds 4,000 output labels and ten rules add 40,000. This makes the multi-rule test a useful stress case, not a pure measurement of matching overhead.
- The single-rule, 1,000-metric result (~0.68 ms) is consistent with the optimized baseline (~0.71 ms), although fixture differences mean the numbers are not strictly interchangeable.

## YAML configuration correspondence

The baseline benchmark corresponds conceptually to a target with one `extended_labels` rule and ten exact `match_label_key_values` entries plus `*`.

The multi-rule fixture corresponds to **multiple items** under `extended_labels`, for example:

```yaml
targets:
  device-a:
    labels:
      - label_key_name: ifDescr
        extended_labels:
          - metric_match: '^ifAdminStatus$'
            match_label_key_values:
              xe-0/2/0:
                label_pairs:
                  neighbor_0: sw1
                  remote_0: xe-0/1/1
              '*':
                label_pairs:
                  scope_0: all
            default_label_pairs:
              discovery_0: CDP
          - metric_match: '^ifAdminStatus$'
            match_label_key_values:
              xe-0/2/0:
                label_pairs:
                  neighbor_1: sw1
                  remote_1: xe-0/1/1
              '*':
                label_pairs:
                  scope_1: all
            default_label_pairs:
              discovery_1: CDP
```

This illustrates two rules and one exact value per rule for brevity. The actual benchmark generates ten exact values for each rule and tests up to ten rules.

## Future benchmark extensions

A complementary benchmark in which **only one of several rules matches** would keep appended-label count approximately constant while increasing rule-evaluation work. That would help distinguish matching overhead from label-slice growth and serialization. Such a benchmark has **not yet been run** and is not included in the results above.

Before optimizing label-slice allocation, compare both all-rules-match and one-rule-matches cases, and use allocation profiling to confirm the source of allocations.