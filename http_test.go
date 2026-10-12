package main

import (
	"bytes"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
)

func compileTargetForTest(
	tb testing.TB,
	target *ExtendedLabelTarget,
) *ExtendedLabelTarget {
	tb.Helper()

	cfg := &ExtendedLabelsConfig{
		Targets: map[string]ExtendedLabelTarget{
			"test": *target,
		},
	}

	if err := cfg.compileMetricMatchers(); err != nil {
		tb.Fatalf("failed to compile test configuration: %v", err)
	}

	compiled := cfg.Targets["test"]
	return &compiled
}

func testMetricFamily(
	metricName string,
	inputLabels map[string]string,
) (*dto.MetricFamily, *dto.Metric) {
	metricType := dto.MetricType_GAUGE
	value := 1.0

	metric := &dto.Metric{
		Gauge: &dto.Gauge{Value: &value},
	}

	for name, labelValue := range inputLabels {
		n, v := name, labelValue
		metric.Label = append(metric.Label, &dto.LabelPair{
			Name: &n, Value: &v,
		})
	}

	return &dto.MetricFamily{
		Name:   &metricName,
		Type:   &metricType,
		Metric: []*dto.Metric{metric},
	}, metric
}

func testLabelValues(metric *dto.Metric) map[string][]string {
	result := make(map[string][]string)
	for _, pair := range metric.Label {
		result[pair.GetName()] = append(
			result[pair.GetName()], pair.GetValue(),
		)
	}
	return result
}

func TestManageExtendedLabelsMultipleRules(t *testing.T) {
	metricName := "ifAdminStatus"
	pattern := "^ifAdminStatus$"
	otherPattern := "^node_cpu.*$"

	tests := []struct {
		name   string
		labels map[string]string
		rules  []ExtendedLabel
		want   map[string][]string
	}{
		{
			name: "multiple rules matching same key",
			labels: map[string]string{
				"ifDescr": "xe-0/2/3",
			},
			rules: []ExtendedLabel{{
				MatchLabelKey: "ifDescr",
				MatchLabels: []MatchLabel{
					{
						MetricMatch: &pattern,
						MatchLabelValues: map[string]*LabelPairs{
							"xe-0/2/3": {
								LabelPair: map[string]string{
									"neighbor": "sw1",
								},
							},
						},
						DefaultLabelPairs: map[string]string{
							"source": "cdp",
						},
					},
					{
						MetricMatch: &pattern,
						MatchLabelValues: map[string]*LabelPairs{
							"*": {
								LabelPair: map[string]string{
									"scope": "network",
								},
							},
						},
						DefaultLabelPairs: map[string]string{
							"discovery": "enabled",
						},
					},
				},
			}},
			want: map[string][]string{
				"neighbor":  {"sw1"},
				"source":    {"cdp"},
				"scope":     {"network"},
				"discovery": {"enabled"},
			},
		},
		{
			name: "multiple matching label keys",
			labels: map[string]string{
				"ifDescr": "xe-0/2/3",
				"device":  "router1",
			},
			rules: []ExtendedLabel{
				{
					MatchLabelKey: "ifDescr",
					MatchLabels: []MatchLabel{{
						MetricMatch: &pattern,
						MatchLabelValues: map[string]*LabelPairs{
							"xe-0/2/3": {
								LabelPair: map[string]string{
									"neighbor": "sw1",
								},
							},
						},
					}},
				},
				{
					MatchLabelKey: "device",
					MatchLabels: []MatchLabel{{
						MetricMatch: &pattern,
						MatchLabelValues: map[string]*LabelPairs{
							"router1": {
								LabelPair: map[string]string{
									"site": "datacenter1",
								},
							},
						},
					}},
				},
			},
			want: map[string][]string{
				"neighbor": {"sw1"},
				"site":     {"datacenter1"},
			},
		},
		{
			name: "only matching metric-name rule applies",
			labels: map[string]string{
				"ifDescr": "xe-0/2/3",
			},
			rules: []ExtendedLabel{{
				MatchLabelKey: "ifDescr",
				MatchLabels: []MatchLabel{
					{
						MetricMatch: &otherPattern,
						MatchLabelValues: map[string]*LabelPairs{
							"*": {
								LabelPair: map[string]string{
									"ignored": "yes",
								},
							},
						},
					},
					{
						MetricMatch: &pattern,
						MatchLabelValues: map[string]*LabelPairs{
							"*": {
								LabelPair: map[string]string{
									"accepted": "yes",
								},
							},
						},
					},
				},
			}},
			want: map[string][]string{
				"accepted": {"yes"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &ExtendedLabelsConfig{
				Targets: map[string]ExtendedLabelTarget{
					"test": {Labels: tt.rules},
				},
			}
			if err := cfg.compileMetricMatchers(); err != nil {
				t.Fatal(err)
			}

			target := cfg.Targets["test"]
			mf, metric := testMetricFamily(metricName, tt.labels)

			var out bytes.Buffer
			if err := (&moduleConfig{}).manageExtendedLabels(
				mf, &target, &out,
			); err != nil {
				t.Fatal(err)
			}

			got := testLabelValues(metric)
			for name, expected := range tt.want {
				actual := got[name]
				if !reflect.DeepEqual(actual, expected) {
					t.Errorf(
						"label %q: got %v, want %v",
						name, actual, expected,
					)
				}
			}

			// Verify no unexpected labels were introduced.
			for name := range got {
				if _, original := tt.labels[name]; original {
					continue
				}
				if _, expected := tt.want[name]; !expected {
					t.Errorf("unexpected label %q: %v", name, got[name])
				}
			}
		})
	}
}

func TestExtendedLabelTargetGetMatchKeys(t *testing.T) {
	// This test verifies that match keys are collected without empty placeholders.
	// The bug this guards against was a slice initialized with len(ee.Labels),
	// which caused empty strings to be inserted before the real keys.
	target := ExtendedLabelTarget{Labels: []ExtendedLabel{
		{MatchLabelKey: "instance"},
		{MatchLabelKey: "hostname"},
	}}

	keys := target.getMatchKeys()
	if len(keys) != 2 {
		t.Fatalf("expected 2 match keys, got %d: %#v", len(keys), keys)
	}
	if keys[0] != "instance" || keys[1] != "hostname" {
		t.Fatalf("unexpected keys: %#v", keys)
	}
}

func TestCompileMetricMatchersCompilesRegex(t *testing.T) {
	// This test validates that metric_match expressions are compiled once and
	// stored on the config objects so the hot path can reuse them.
	pattern := "^node_.*$"
	labelConfig := &ExtendedLabelsConfig{
		Targets: map[string]ExtendedLabelTarget{
			"device-a": {
				Labels: []ExtendedLabel{{
					MatchLabelKey: "instance",
					MatchLabels: []MatchLabel{{
						MetricMatch: &pattern,
					}},
				}},
			},
		},
	}

	if err := labelConfig.compileMetricMatchers(); err != nil {
		t.Fatalf("compileMetricMatchers returned error: %v", err)
	}

	compiled := labelConfig.Targets["device-a"].Labels[0].MatchLabels[0].metricMatchRegexp
	if compiled == nil {
		t.Fatal("expected compiled metric_match regex to be set")
	}
}

func TestCompileMetricMatchersRejectsInvalidRegex(t *testing.T) {
	// This test ensures invalid metric_match patterns fail early at config load
	// time, instead of failing repeatedly during metric processing.
	invalidPattern := "(["
	labelConfig := &ExtendedLabelsConfig{
		Targets: map[string]ExtendedLabelTarget{
			"device-a": {
				Labels: []ExtendedLabel{{
					MatchLabelKey: "instance",
					MatchLabels: []MatchLabel{{
						MetricMatch: &invalidPattern,
					}},
				}},
			},
		},
	}

	if err := labelConfig.compileMetricMatchers(); err == nil {
		t.Fatal("expected compileMetricMatchers to fail for invalid regex")
	}
}

func TestManageExtendedLabelsAddsMatchingLabelsSnmpMetrics(t *testing.T) {
	// This test exercises the main extension path: when the target label exists,
	// the metric name matches, and the value matches a configured rule, the
	// additional labels from both the specific match and default labels must be
	// appended to the metric.
	metricName := "ifAdminStatus"
	instanceName := "ifDescr"
	instanceValue := "xe-0/2/3"
	metricValue := 42.0
	metricType := dto.MetricType_GAUGE
	metric := &dto.Metric{
		Label: []*dto.LabelPair{{Name: &instanceName, Value: &instanceValue}},
		Gauge: &dto.Gauge{Value: &metricValue},
	}
	mf := &dto.MetricFamily{
		Name:   &metricName,
		Type:   &metricType,
		Metric: []*dto.Metric{metric},
	}

	pattern := "ifAdminStatus"
	target := &ExtendedLabelTarget{Labels: []ExtendedLabel{{
		MatchLabelKey: "ifDescr",
		MatchLabels: []MatchLabel{{
			MetricMatch: &pattern,
			MatchLabelValues: map[string]*LabelPairs{
				instanceValue: {LabelPair: map[string]string{"discovery": "CDP", "neighbor": "sw1.foo.com", "remote_interface": "xe-0/1/1"}},
			},
		}},
	}}}

	var out bytes.Buffer
	target = compileTargetForTest(t, target)
	if err := (&moduleConfig{}).manageExtendedLabels(mf, target, &out); err != nil {
		t.Fatalf("manageExtendedLabels returned error: %v", err)
	}

	labels := map[string]string{}
	for _, label := range metric.Label {
		labels[label.GetName()] = label.GetValue()
	}

	if labels["discovery"] != "CDP" {
		t.Fatalf("expected region label to be added, got %#v", labels)
	}
}

func TestManageExtendedLabelsNoMatchSnmpMetrics(t *testing.T) {
	// This test exercises the main extension path: when the target label exists,
	// the metric name matches, and the value matches a configured rule, the
	// additional labels from both the specific match and default labels must be
	// appended to the metric.
	metricName := "ifAdminStatus"
	instanceName := "ifDescr"
	instanceValue := "xe-0/2/5"
	metricValue := 42.0
	metricType := dto.MetricType_GAUGE
	metric := &dto.Metric{
		Label: []*dto.LabelPair{{Name: &instanceName, Value: &instanceValue}},
		Gauge: &dto.Gauge{Value: &metricValue},
	}
	mf := &dto.MetricFamily{
		Name:   &metricName,
		Type:   &metricType,
		Metric: []*dto.Metric{metric},
	}

	pattern := "ifAdminStatus"
	target := &ExtendedLabelTarget{Labels: []ExtendedLabel{{
		MatchLabelKey: "ifDescr",
		MatchLabels: []MatchLabel{{
			MetricMatch: &pattern,
			// xe-0/2/3 no matching
			MatchLabelValues: map[string]*LabelPairs{
				"xe-0/2/3": {LabelPair: map[string]string{"discovery": "CDP", "neighbor": "sw1.foo.com", "remote_interface": "xe-0/1/1"}},
			},
		}},
	}}}

	var out bytes.Buffer
	target = compileTargetForTest(t, target)
	if err := (&moduleConfig{}).manageExtendedLabels(mf, target, &out); err != nil {
		t.Fatalf("manageExtendedLabels returned error: %v", err)
	}

	labels := map[string]string{}
	for _, label := range metric.Label {
		labels[label.GetName()] = label.GetValue()
	}

	if _, exists := labels["discovery"]; exists {
		t.Fatalf("expected no discovery label to be added, got %#v", labels)
	}
}

func TestManageExtendedLabelsAddsMatchingLabels(t *testing.T) {
	// This test exercises the main extension path: when the target label exists,
	// the metric name matches, and the value matches a configured rule, the
	// additional labels from both the specific match and default labels must be
	// appended to the metric.
	metricName := "node_cpu_seconds_total"
	instanceName := "instance"
	instanceValue := "node-1"
	metricValue := 42.0
	metricType := dto.MetricType_GAUGE
	metric := &dto.Metric{
		Label: []*dto.LabelPair{{Name: &instanceName, Value: &instanceValue}},
		Gauge: &dto.Gauge{Value: &metricValue},
	}
	mf := &dto.MetricFamily{
		Name:   &metricName,
		Type:   &metricType,
		Metric: []*dto.Metric{metric},
	}

	pattern := "^node_.*$"
	target := &ExtendedLabelTarget{Labels: []ExtendedLabel{{
		MatchLabelKey: "instance",
		MatchLabels: []MatchLabel{{
			MetricMatch: &pattern,
			MatchLabelValues: map[string]*LabelPairs{
				instanceValue: {LabelPair: map[string]string{"region": "eu-west-1"}},
			},
			DefaultLabelPairs: map[string]string{"source": "proxy"},
		}},
	}}}

	var out bytes.Buffer
	target = compileTargetForTest(t, target)
	if err := (&moduleConfig{}).manageExtendedLabels(mf, target, &out); err != nil {
		t.Fatalf("manageExtendedLabels returned error: %v", err)
	}

	labels := map[string]string{}
	for _, label := range metric.Label {
		labels[label.GetName()] = label.GetValue()
	}

	if labels["region"] != "eu-west-1" {
		t.Fatalf("expected region label to be added, got %#v", labels)
	}
	if labels["source"] != "proxy" {
		t.Fatalf("expected source label to be added, got %#v", labels)
	}
}

func TestManageExtendedLabelsSkipsNonMatchingMetricName(t *testing.T) {
	// This test ensures that metric-name filtering is enforced before adding any
	// extended labels. A non-matching metric should not receive labels even when
	// the target label and value have a configured rule.
	metricName := "http_requests_total"
	instanceName := "instance"
	instanceValue := "node-1"
	metricValue := 42.0
	metricType := dto.MetricType_COUNTER
	metric := &dto.Metric{
		Label:   []*dto.LabelPair{{Name: &instanceName, Value: &instanceValue}},
		Counter: &dto.Counter{Value: &metricValue},
	}
	mf := &dto.MetricFamily{
		Name:   &metricName,
		Type:   &metricType,
		Metric: []*dto.Metric{metric},
	}

	pattern := "^node_.*$"
	target := &ExtendedLabelTarget{Labels: []ExtendedLabel{{
		MatchLabelKey: "instance",
		MatchLabels: []MatchLabel{{
			MetricMatch: &pattern,
			MatchLabelValues: map[string]*LabelPairs{
				instanceValue: {LabelPair: map[string]string{"region": "eu-west-1"}},
			},
		}},
	}}}

	var out bytes.Buffer
	target = compileTargetForTest(t, target)
	if err := (&moduleConfig{}).manageExtendedLabels(mf, target, &out); err != nil {
		t.Fatalf("manageExtendedLabels returned error: %v", err)
	}

	for _, label := range metric.Label {
		if label.GetName() == "region" {
			t.Fatalf("expected no region label for non-matching metric name, got %#v", metric.Label)
		}
	}
}

func TestManageExtendedLabelsAppliesWildcardRule(t *testing.T) {
	// This test verifies the O(1) wildcard lookup path: when no exact key is
	// configured for the metric value, the "*" rule is still applied.
	metricName := "node_memory_MemAvailable_bytes"
	instanceName := "instance"
	instanceValue := "unlisted-node"
	metricValue := 42.0
	metricType := dto.MetricType_GAUGE
	metric := &dto.Metric{
		Label: []*dto.LabelPair{{Name: &instanceName, Value: &instanceValue}},
		Gauge: &dto.Gauge{Value: &metricValue},
	}
	mf := &dto.MetricFamily{
		Name:   &metricName,
		Type:   &metricType,
		Metric: []*dto.Metric{metric},
	}

	pattern := "^node_.*$"
	target := &ExtendedLabelTarget{Labels: []ExtendedLabel{{
		MatchLabelKey: "instance",
		MatchLabels: []MatchLabel{{
			MetricMatch: &pattern,
			MatchLabelValues: map[string]*LabelPairs{
				"*": {LabelPair: map[string]string{"scope": "all"}},
			},
		}},
	}}}

	var out bytes.Buffer
	target = compileTargetForTest(t, target)
	if err := (&moduleConfig{}).manageExtendedLabels(mf, target, &out); err != nil {
		t.Fatalf("manageExtendedLabels returned error: %v", err)
	}

	labels := map[string]string{}
	for _, label := range metric.Label {
		labels[label.GetName()] = label.GetValue()
	}
	if labels["scope"] != "all" {
		t.Fatalf("expected wildcard rule to add scope label, got %#v", labels)
	}
}

func TestManageExtendedLabelsAddsDefaultLabelsOnceWhenExactAndWildcardMatch(t *testing.T) {
	// This test ensures default labels are appended once per MatchLabel even
	// when both the exact-value and wildcard rules match the same metric.
	metricName := "node_memory_MemAvailable_bytes"
	instanceName := "instance"
	instanceValue := "node-1"
	metricValue := 42.0
	metricType := dto.MetricType_GAUGE
	metric := &dto.Metric{
		Label: []*dto.LabelPair{{Name: &instanceName, Value: &instanceValue}},
		Gauge: &dto.Gauge{Value: &metricValue},
	}
	mf := &dto.MetricFamily{
		Name:   &metricName,
		Type:   &metricType,
		Metric: []*dto.Metric{metric},
	}

	pattern := "^node_.*$"
	target := &ExtendedLabelTarget{Labels: []ExtendedLabel{{
		MatchLabelKey: "instance",
		MatchLabels: []MatchLabel{{
			MetricMatch: &pattern,
			MatchLabelValues: map[string]*LabelPairs{
				instanceValue: {LabelPair: map[string]string{"region": "eu-west-1"}},
				"*":           {LabelPair: map[string]string{"scope": "all"}},
			},
			DefaultLabelPairs: map[string]string{"source": "proxy"},
		}},
	}}}

	var out bytes.Buffer
	target = compileTargetForTest(t, target)
	if err := (&moduleConfig{}).manageExtendedLabels(mf, target, &out); err != nil {
		t.Fatalf("manageExtendedLabels returned error: %v", err)
	}

	sourceLabelCount := 0
	labels := map[string]string{}
	for _, label := range metric.Label {
		labels[label.GetName()] = label.GetValue()
		if label.GetName() == "source" {
			sourceLabelCount++
		}
	}

	if labels["region"] != "eu-west-1" {
		t.Fatalf("expected exact-match label to be added, got %#v", labels)
	}
	if labels["scope"] != "all" {
		t.Fatalf("expected wildcard label to be added, got %#v", labels)
	}
	if labels["source"] != "proxy" {
		t.Fatalf("expected default label to be added, got %#v", labels)
	}
	if sourceLabelCount != 1 {
		t.Fatalf("expected default label to be added once, got %d occurrences in %#v", sourceLabelCount, metric.Label)
	}
}

func TestManageExtendedLabelsHandlesMixedMetricLabelShapes(t *testing.T) {
	// This test validates the pre-bound label-index optimization safety: if the
	// first metric defines the label used for binding, later metrics with a
	// different label shape must not panic and should still be processed safely.
	metricName := "ifAdminStatus"
	metricType := dto.MetricType_GAUGE
	matchLabelName := "ifDescr"
	firstLabelValue := "xe-0/2/3"
	secondLabelName := "otherLabel"
	secondLabelValue := "no-match"
	firstMetricValue := 1.0
	secondMetricValue := 2.0

	firstMetric := &dto.Metric{
		Label: []*dto.LabelPair{{Name: &matchLabelName, Value: &firstLabelValue}},
		Gauge: &dto.Gauge{Value: &firstMetricValue},
	}
	secondMetric := &dto.Metric{
		Label: []*dto.LabelPair{{Name: &secondLabelName, Value: &secondLabelValue}},
		Gauge: &dto.Gauge{Value: &secondMetricValue},
	}
	mf := &dto.MetricFamily{
		Name:   &metricName,
		Type:   &metricType,
		Metric: []*dto.Metric{firstMetric, secondMetric},
	}

	pattern := "ifAdminStatus"
	target := &ExtendedLabelTarget{Labels: []ExtendedLabel{{
		MatchLabelKey: matchLabelName,
		MatchLabels: []MatchLabel{{
			MetricMatch: &pattern,
			MatchLabelValues: map[string]*LabelPairs{
				firstLabelValue: {LabelPair: map[string]string{"discovery": "CDP"}},
			},
		}},
	}}}

	var out bytes.Buffer
	target = compileTargetForTest(t, target)
	if err := (&moduleConfig{}).manageExtendedLabels(mf, target, &out); err != nil {
		t.Fatalf("manageExtendedLabels returned error: %v", err)
	}

	firstLabels := map[string]string{}
	for _, label := range firstMetric.Label {
		firstLabels[label.GetName()] = label.GetValue()
	}
	if firstLabels["discovery"] != "CDP" {
		t.Fatalf("expected first metric to be extended, got %#v", firstLabels)
	}

	for _, label := range secondMetric.Label {
		if label.GetName() == "discovery" {
			t.Fatalf("expected second metric to remain unextended, got %#v", secondMetric.Label)
		}
	}
}

func TestManageExtendedLabelsHandlesNilLabelSliceAfterBinding(t *testing.T) {
	// This test ensures that a nil label slice on a later metric does not panic
	// after indices were bound from the first metric.
	metricName := "ifAdminStatus"
	metricType := dto.MetricType_GAUGE
	matchLabelName := "ifDescr"
	firstLabelValue := "xe-0/2/3"
	firstMetricValue := 1.0
	secondMetricValue := 2.0

	firstMetric := &dto.Metric{
		Label: []*dto.LabelPair{{Name: &matchLabelName, Value: &firstLabelValue}},
		Gauge: &dto.Gauge{Value: &firstMetricValue},
	}
	secondMetric := &dto.Metric{
		Label: nil,
		Gauge: &dto.Gauge{Value: &secondMetricValue},
	}
	mf := &dto.MetricFamily{
		Name:   &metricName,
		Type:   &metricType,
		Metric: []*dto.Metric{firstMetric, secondMetric},
	}

	pattern := "ifAdminStatus"
	target := &ExtendedLabelTarget{Labels: []ExtendedLabel{{
		MatchLabelKey: matchLabelName,
		MatchLabels: []MatchLabel{{
			MetricMatch: &pattern,
			MatchLabelValues: map[string]*LabelPairs{
				firstLabelValue: {LabelPair: map[string]string{"discovery": "CDP"}},
			},
		}},
	}}}

	var out bytes.Buffer
	target = compileTargetForTest(t, target)
	if err := (&moduleConfig{}).manageExtendedLabels(mf, target, &out); err != nil {
		t.Fatalf("manageExtendedLabels returned error: %v", err)
	}

	firstLabels := map[string]string{}
	for _, label := range firstMetric.Label {
		firstLabels[label.GetName()] = label.GetValue()
	}
	if firstLabels["discovery"] != "CDP" {
		t.Fatalf("expected first metric to be extended, got %#v", firstLabels)
	}
	if len(secondMetric.Label) != 0 {
		t.Fatalf("expected second metric labels to stay empty, got %#v", secondMetric.Label)
	}
}

func TestManageExtendedLabelsBindsFromFirstUsableMetric(t *testing.T) {
	// This test verifies that binding is delayed until the first metric with
	// labels is found. A nil-label first metric should no longer stop processing.
	metricName := "ifAdminStatus"
	metricType := dto.MetricType_GAUGE
	matchLabelName := "ifDescr"
	matchLabelValue := "xe-0/2/3"
	emptyMetricValue := 1.0
	matchingMetricValue := 2.0

	firstMetric := &dto.Metric{
		Label: nil,
		Gauge: &dto.Gauge{Value: &emptyMetricValue},
	}
	secondMetric := &dto.Metric{
		Label: []*dto.LabelPair{{Name: &matchLabelName, Value: &matchLabelValue}},
		Gauge: &dto.Gauge{Value: &matchingMetricValue},
	}
	mf := &dto.MetricFamily{
		Name:   &metricName,
		Type:   &metricType,
		Metric: []*dto.Metric{firstMetric, secondMetric},
	}

	pattern := "ifAdminStatus"
	target := &ExtendedLabelTarget{Labels: []ExtendedLabel{{
		MatchLabelKey: matchLabelName,
		MatchLabels: []MatchLabel{{
			MetricMatch: &pattern,
			MatchLabelValues: map[string]*LabelPairs{
				matchLabelValue: {LabelPair: map[string]string{"discovery": "CDP"}},
			},
		}},
	}}}

	var out bytes.Buffer
	target = compileTargetForTest(t, target)
	if err := (&moduleConfig{}).manageExtendedLabels(mf, target, &out); err != nil {
		t.Fatalf("manageExtendedLabels returned error: %v", err)
	}

	labels := map[string]string{}
	for _, label := range secondMetric.Label {
		labels[label.GetName()] = label.GetValue()
	}
	if labels["discovery"] != "CDP" {
		t.Fatalf("expected second metric to be extended, got %#v", labels)
	}
}

func buildMultiRuleBenchmarkFixture(
	metricsCount int,
	matchKeyCount int,
	rulesPerKey int,
) (*dto.MetricFamily, *ExtendedLabelTarget) {
	metricName := "ifAdminStatus"
	metricType := dto.MetricType_GAUGE
	matchLabelName := "ifDescr"

	metrics := make([]*dto.Metric, metricsCount)

	for i := range metrics {
		labelValue := fmt.Sprintf(
			"xe-0/2/%d", i%matchKeyCount,
		)
		value := float64(i)

		metrics[i] = &dto.Metric{
			Label: []*dto.LabelPair{{
				Name:  &matchLabelName,
				Value: &labelValue,
			}},
			Gauge: &dto.Gauge{Value: &value},
		}
	}

	rules := make([]MatchLabel, rulesPerKey)
	metricPattern := "^ifAdminStatus$"

	for r := range rules {
		matchValues := make(
			map[string]*LabelPairs, matchKeyCount+1,
		)

		for k := 0; k < matchKeyCount; k++ {
			key := fmt.Sprintf("xe-0/2/%d", k)

			matchValues[key] = &LabelPairs{
				LabelPair: map[string]string{
					fmt.Sprintf("neighbor_%d", r): "sw1",
					fmt.Sprintf("remote_%d", r):   "xe-0/1/1",
				},
			}
		}

		matchValues["*"] = &LabelPairs{
			LabelPair: map[string]string{
				fmt.Sprintf("scope_%d", r): "all",
			},
		}

		rules[r] = MatchLabel{
			MetricMatch:      &metricPattern,
			MatchLabelValues: matchValues,
			DefaultLabelPairs: map[string]string{
				fmt.Sprintf("discovery_%d", r): "CDP",
			},
		}
	}

	cfg := &ExtendedLabelsConfig{
		Targets: map[string]ExtendedLabelTarget{
			"test": {
				Labels: []ExtendedLabel{{
					MatchLabelKey: matchLabelName,
					MatchLabels:   rules,
				}},
			},
		},
	}

	if err := cfg.compileMetricMatchers(); err != nil {
		panic(err)
	}

	target := cfg.Targets["test"]

	return &dto.MetricFamily{
		Name:   &metricName,
		Type:   &metricType,
		Metric: metrics,
	}, &target
}

func BenchmarkManageExtendedLabelsMultipleRules(b *testing.B) {
	metricCounts := []int{100, 1000}
	ruleCounts := []int{1, 2, 5, 10}

	for _, metricsCount := range metricCounts {
		for _, rulesCount := range ruleCounts {
			name := fmt.Sprintf(
				"metrics-%d-rules-%d",
				metricsCount, rulesCount,
			)

			b.Run(name, func(b *testing.B) {
				baseMF, target := buildMultiRuleBenchmarkFixture(
					metricsCount, 10, rulesCount,
				)

				b.ReportAllocs()
				b.ResetTimer()

				for i := 0; i < b.N; i++ {
					b.StopTimer()
					mf := cloneMetricFamilyForBenchmark(baseMF)
					var out bytes.Buffer
					b.StartTimer()

					if err := (&moduleConfig{}).manageExtendedLabels(
						mf, target, &out,
					); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

func BenchmarkManageExtendedLabels(b *testing.B) {
	benchmarkCases := []struct {
		name          string
		metricsCount  int
		matchKeyCount int
	}{
		{name: "metrics-100-match-keys-10", metricsCount: 100, matchKeyCount: 10},
		{name: "metrics-1000-match-keys-10", metricsCount: 1000, matchKeyCount: 10},
	}

	for _, benchCase := range benchmarkCases {
		b.Run(benchCase.name, func(b *testing.B) {
			baseMetricFamily, labelTarget := buildManageExtendedLabelsBenchmarkFixture(benchCase.metricsCount, benchCase.matchKeyCount)

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				// Benchmark only the function under test. Cloning is benchmark setup
				// work to provide a fresh mutable input and is intentionally excluded.
				b.StopTimer()
				metricFamily := cloneMetricFamilyForBenchmark(baseMetricFamily)
				var out bytes.Buffer
				b.StartTimer()
				labelTarget = compileTargetForTest(b, labelTarget)
				if err := (&moduleConfig{}).manageExtendedLabels(metricFamily, labelTarget, &out); err != nil {
					b.Fatalf("manageExtendedLabels returned error: %v", err)
				}
			}
		})
	}
}

func buildManageExtendedLabelsBenchmarkFixture(metricsCount int, matchKeyCount int) (*dto.MetricFamily, *ExtendedLabelTarget) {
	metricName := "ifAdminStatus"
	matchLabelName := "ifDescr"
	metricType := dto.MetricType_GAUGE

	metrics := make([]*dto.Metric, metricsCount)
	for i := 0; i < metricsCount; i++ {
		labelValue := fmt.Sprintf("xe-0/2/%d", i%matchKeyCount)
		metricValue := float64(i)
		metrics[i] = &dto.Metric{
			Label: []*dto.LabelPair{{Name: &matchLabelName, Value: &labelValue}},
			Gauge: &dto.Gauge{Value: &metricValue},
		}
	}

	matchLabelValues := make(map[string]*LabelPairs, matchKeyCount+1)
	for i := 0; i < matchKeyCount; i++ {
		interfaceName := fmt.Sprintf("xe-0/2/%d", i)
		matchLabelValues[interfaceName] = &LabelPairs{
			LabelPair: map[string]string{
				"neighbor":         "sw1.foo.com",
				"remote_interface": "xe-0/1/1",
			},
		}
	}
	matchLabelValues["*"] = &LabelPairs{
		LabelPair: map[string]string{
			"scope": "all",
		},
	}

	metricMatchPattern := "^ifAdminStatus$"
	targets := map[string]ExtendedLabelTarget{
		"device-a": {
			Labels: []ExtendedLabel{{
				MatchLabelKey: matchLabelName,
				MatchLabels: []MatchLabel{{
					MetricMatch:       &metricMatchPattern,
					MatchLabelValues:  matchLabelValues,
					DefaultLabelPairs: map[string]string{"discovery": "CDP"},
				}},
			}},
		},
	}

	labelConfig := &ExtendedLabelsConfig{Targets: targets}
	if err := labelConfig.compileMetricMatchers(); err != nil {
		panic(fmt.Sprintf("failed to compile benchmark matchers: %v", err))
	}
	target := labelConfig.Targets["device-a"]
	return &dto.MetricFamily{Name: &metricName, Type: &metricType, Metric: metrics}, &target
}

func cloneMetricFamilyForBenchmark(source *dto.MetricFamily) *dto.MetricFamily {
	metricType := source.GetType()
	metricName := source.GetName()
	cloned := &dto.MetricFamily{
		Name:   &metricName,
		Type:   &metricType,
		Metric: make([]*dto.Metric, len(source.Metric)),
	}

	for index, originalMetric := range source.Metric {
		metricCopy := &dto.Metric{
			Gauge:       originalMetric.Gauge,
			Counter:     originalMetric.Counter,
			Summary:     originalMetric.Summary,
			Untyped:     originalMetric.Untyped,
			Histogram:   originalMetric.Histogram,
			TimestampMs: originalMetric.TimestampMs,
		}
		labelsCopy := make([]*dto.LabelPair, len(originalMetric.Label))
		for labelIndex, originalLabel := range originalMetric.Label {
			name := originalLabel.GetName()
			value := originalLabel.GetValue()
			labelsCopy[labelIndex] = &dto.LabelPair{Name: &name, Value: &value}
		}
		metricCopy.Label = labelsCopy
		cloned.Metric[index] = metricCopy
	}

	return cloned
}

func BenchmarkReverseProxyHandler(b *testing.B) {
	body := genRandomMetricsResponse(10000, 10)

	testExporter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reader := bytes.NewReader(body.Bytes())
		io.Copy(w, reader)
	}))
	defer testExporter.Close()

	URL, _ := url.Parse(testExporter.URL)
	verify := true
	port, _ := strconv.ParseInt(URL.Port(), 0, 0)
	modCfg := &moduleConfig{
		Method:  "http",
		Timeout: 5 * time.Second,
		HTTP: &httpConfig{
			Verify:  &verify,
			Scheme:  URL.Scheme,
			Address: URL.Hostname(),
			Port:    int(port),
			Path:    "/",
		},
	}

	if err := checkModuleConfig("test", modCfg); err != nil {
		b.Fatalf("Failed to check module config: %v", err)
	}

	cfg := &config{
		Modules: map[string]*moduleConfig{
			"test": modCfg,
		},
	}

	req := httptest.NewRequest("GET", "/proxy?module=test", nil)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rr := httptest.NewRecorder()
		cfg.doProxy(rr, req)
		if rr.Code != http.StatusOK {
			b.Fatalf("Bad response status %d", rr.Code)
		}
		if len(rr.Body.Bytes()) <= 0 {
			b.Fatal("Response body is absent")
		}
	}
}

// genRandomMetricsResponse generates http response body which contains random set of
// prometheus metrics. mf_num sets number of metric families in response which has
// metric names in format 'metric{random number}'. m_num controls number of metrics
// inside each metric family. Metrics inside metric families differ in values of
// label 'label'.
func genRandomMetricsResponse(mfNum int, mNum int) *bytes.Buffer {
	rand.Seed(time.Now().UnixNano())
	helpMsg := "help msg"
	labelName := "label"
	metricFamilies := make([]*dto.MetricFamily, mfNum)
	metricType := dto.MetricType_GAUGE
	for i := range metricFamilies {
		metrics := make([]*dto.Metric, mNum)
		for i := range metrics {
			labelValue := fmt.Sprint(rand.Int63())
			value := rand.Float64()
			ts := time.Now().UnixNano()
			metrics[i] = &dto.Metric{
				Label: []*dto.LabelPair{
					{
						Name:  &labelName,
						Value: &labelValue,
					},
				},
				Gauge: &dto.Gauge{
					Value: &value,
				},
				TimestampMs: &ts,
			}
		}
		metricName := fmt.Sprintf("metric%d", rand.Int63())
		metricFamilies[i] = &dto.MetricFamily{
			Name:   &metricName,
			Help:   &helpMsg,
			Type:   &metricType,
			Metric: metrics,
		}
	}

	buf := &bytes.Buffer{}
	enc := expfmt.NewEncoder(buf, expfmt.FmtText)
	for _, mf := range metricFamilies {
		enc.Encode(mf)
	}

	return buf
}
