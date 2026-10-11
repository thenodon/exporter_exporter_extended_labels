# Benchmark guide: `BenchmarkManageExtendedLabels`

This document explains the benchmark added for `manageExtendedLabels`, how to run it, how to read the output, and how the benchmark fixture maps to YAML config.

## What is being benchmarked

The benchmark measures this hot path:

- `moduleConfig.manageExtendedLabels(...)`

The benchmark is focused on label-extension logic only (matching + label appends + text serialization done by that function).

## Where the benchmark lives

- File: `http_test.go`
- Benchmark: `BenchmarkManageExtendedLabels`

It runs multiple sub-benchmarks, for example:

- `metrics-100-match-keys-10`
- `metrics-1000-match-keys-10`

## What the case names mean

`metrics-100-match-keys-10` means:

- `metrics-100`: the `MetricFamily` contains 100 metric samples.
- `match-keys-10`: the rule table has 10 exact keys in `match_label_key_values` (plus `*` wildcard).

Important: this does **not** mean 100 different metric names.  
In the fixture, all samples are in the same metric family (`ifAdminStatus`), each with varying `ifDescr` values.

## How the benchmark runs

Each benchmark case:

1. Builds a reusable fixture (`MetricFamily` + `ExtendedLabelTarget`).
2. For each benchmark iteration, clones the metric family to get a fresh mutable input.
3. Calls `manageExtendedLabels` with that cloned input.

The cloning/setup step is intentionally excluded from timed benchmark measurements with `b.StopTimer()` / `b.StartTimer()`, so `ns/op` reflects the function under test rather than fixture preparation overhead.

## How to run

Run benchmark with allocation stats:

```bash
go test -run=^$ -bench BenchmarkManageExtendedLabels -benchmem ./...
```

Run benchmark and produce CPU profile:

```bash
go test -run=^$ -bench BenchmarkManageExtendedLabels -cpuprofile cpu.out ./...
go tool pprof -http=127.0.0.1:8080 cpu.out
```

## How to read the output

Example output:

```text
BenchmarkManageExtendedLabels/metrics-100-match-keys-10-20      8718    128120 ns/op    81971 B/op    1512 allocs/op
BenchmarkManageExtendedLabels/metrics-1000-match-keys-10-20      788   1697766 ns/op   752662 B/op   15018 allocs/op
```

Column meanings:

- `8718` / `788`: number of iterations (`b.N`) used for stable timing.
- `ns/op`: average nanoseconds per benchmark operation (one `manageExtendedLabels` call).
- `B/op`: average heap bytes allocated per operation.
- `allocs/op`: average allocation count per operation.
- trailing `-20`: `GOMAXPROCS` used in that run.

Quick interpretation:

- Lower `ns/op` => faster.
- Lower `B/op` and `allocs/op` => less allocation and usually lower GC pressure.

## YAML equivalent of the benchmark config

The benchmark fixture is programmatically created, but conceptually corresponds to a config like:

```yaml
targets:
  device-a:
    labels:
      - label_key_name: ifDescr
        extended_labels:
          - metric_match: "^ifAdminStatus$"
            match_label_key_values:
              xe-0/2/0:
                label_pairs:
                  neighbor: sw1.foo.com
                  remote_interface: xe-0/1/1
              xe-0/2/1:
                label_pairs:
                  neighbor: sw1.foo.com
                  remote_interface: xe-0/1/1
              xe-0/2/2:
                label_pairs:
                  neighbor: sw1.foo.com
                  remote_interface: xe-0/1/1
              xe-0/2/3:
                label_pairs:
                  neighbor: sw1.foo.com
                  remote_interface: xe-0/1/1
              xe-0/2/4:
                label_pairs:
                  neighbor: sw1.foo.com
                  remote_interface: xe-0/1/1
              xe-0/2/5:
                label_pairs:
                  neighbor: sw1.foo.com
                  remote_interface: xe-0/1/1
              xe-0/2/6:
                label_pairs:
                  neighbor: sw1.foo.com
                  remote_interface: xe-0/1/1
              xe-0/2/7:
                label_pairs:
                  neighbor: sw1.foo.com
                  remote_interface: xe-0/1/1
              xe-0/2/8:
                label_pairs:
                  neighbor: sw1.foo.com
                  remote_interface: xe-0/1/1
              xe-0/2/9:
                label_pairs:
                  neighbor: sw1.foo.com
                  remote_interface: xe-0/1/1
              "*":
                label_pairs:
                  scope: all
            default_label_pairs:
              discovery: CDP
```

## Mapping between YAML and benchmark internals

- `label_key_name: ifDescr` -> label used for match lookup.
- `metric_match: "^ifAdminStatus$"` -> metric-family name filter.
- `match_label_key_values` exact keys -> direct O(1) exact lookup.
- `"*"` key -> wildcard rule lookup.
- `default_label_pairs` -> labels appended once when any exact/wildcard rule matched.

