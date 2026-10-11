package main

import (
	"bytes"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
)

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
