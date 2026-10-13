// Copyright 2016 Qubit Ltd.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	log "github.com/sirupsen/logrus"
)

const (
	// VerificationErrorMsg to send in response body when verification of proxied server
	// response is failed
	VerificationErrorMsg = "Internal Server Error: " +
		"Response from proxied server failed verification. " +
		"See server logs for details"
)

// VerifyError is an error type that supports reporting verification errors
type VerifyError struct {
	msg   string
	cause error
}

func (e *VerifyError) Error() string { return e.msg + ": " + e.cause.Error() }
func (e *VerifyError) Unwrap() error { return e.cause }

func (cfg moduleConfig) getReverseProxyDirectorFunc() (func(*http.Request), error) {
	base, err := url.Parse(cfg.HTTP.Path)
	if err != nil {
		return nil, fmt.Errorf("http configuration path should be a valid URL path with options, %w", err)
	}

	cvs := base.Query()

	return func(r *http.Request) {
		qvs := r.URL.Query()
		for k, vs := range cvs {
			for _, v := range vs {
				qvs.Add(k, v)
			}
		}
		qvs["module"] = qvs["module"][1:]

		r.URL.RawQuery = qvs.Encode()

		for k, v := range cfg.HTTP.Headers {
			r.Header.Add(k, v)
		}

		r.URL.Scheme = cfg.HTTP.Scheme
		r.URL.Host = net.JoinHostPort(cfg.HTTP.Address, strconv.Itoa(cfg.HTTP.Port))
		if _, ok := cfg.HTTP.Headers["host"]; ok {
			r.Host = cfg.HTTP.Headers["host"]
		}
		r.URL.Path = base.Path
		if cfg.HTTP.BasicAuthUsername != "" && cfg.HTTP.BasicAuthPassword != "" {
			r.SetBasicAuth(cfg.HTTP.BasicAuthUsername, cfg.HTTP.BasicAuthPassword)
		}
	}, nil
}

func (cfg moduleConfig) getReverseProxyModifyResponseFunc() func(*http.Response) error {
	return func(resp *http.Response) error {
		if resp.StatusCode != 200 {
			return nil
		}

		var (
			err     error
			body    bytes.Buffer
			oldBody = resp.Body
		)
		defer oldBody.Close()

		if _, err = body.ReadFrom(oldBody); err != nil {
			return &VerifyError{"Failed to read body from proxied server", err}
		}

		resp.Body = io.NopCloser(bytes.NewReader(body.Bytes()))

		var bodyReader io.ReadCloser
		if resp.Header.Get("Content-Encoding") == "gzip" {
			bodyReader, err = gzip.NewReader(bytes.NewReader(body.Bytes()))
			if err != nil {
				return &VerifyError{"Failed to decode gzipped response", err}
			}
		} else {
			bodyReader = io.NopCloser(bytes.NewReader(body.Bytes()))
		}
		defer bodyReader.Close()

		dec := expfmt.NewDecoder(bodyReader, expfmt.ResponseFormat(resp.Header))
		for {
			mf := dto.MetricFamily{}
			err := dec.Decode(&mf)
			if err == io.EOF {
				break
			}
			if err != nil {
				proxyMalformedCount.WithLabelValues(cfg.name).Inc()
				return &VerifyError{"Failed to decode metrics from proxied server", err}
			}
		}

		return nil
	}
}

// getLabelExtendReverseProxyModifyResponseFunc returns a function that modifies the response from the proxied server
// by extending labels on metrics based on the configuration.
func (cfg moduleConfig) getLabelExtendReverseProxyModifyResponseFunc() func(*http.Response) error {
	return func(resp *http.Response) error {
		if resp.StatusCode != 200 {
			return nil
		}

		// Since the body is extended the header Content-Length must be removed
		resp.Header.Del("Content-Length")

		var (
			err     error
			body    bytes.Buffer
			oldBody = resp.Body
		)
		defer oldBody.Close()

		if _, err = body.ReadFrom(oldBody); err != nil {
			return &VerifyError{"Failed to read body from proxied server", err}
		}

		resp.Body = io.NopCloser(bytes.NewReader(body.Bytes()))

		var bodyReader io.ReadCloser
		if resp.Header.Get("Content-Encoding") == "gzip" {
			bodyReader, err = gzip.NewReader(bytes.NewReader(body.Bytes()))
			if err != nil {
				return &VerifyError{"Failed to decode gzipped response", err}
			}
		} else {
			bodyReader = io.NopCloser(bytes.NewReader(body.Bytes()))
		}
		defer bodyReader.Close()

		dec := expfmt.NewDecoder(bodyReader, expfmt.ResponseFormat(resp.Header))

		out := &bytes.Buffer{}

		for {
			mf := dto.MetricFamily{}

			err := dec.Decode(&mf)
			if err == io.EOF {
				break
			}

			if err != nil {
				proxyMalformedCount.WithLabelValues(cfg.name).Inc()
				return &VerifyError{"Failed to decode metrics from proxied server", err}
			}

			// Validate that the target name exists in the configuration file
			// url include target=device1.foo.com
			labelKeyNames := getTargetMatch(resp.Request, cfg)
			if labelKeyNames == nil {
				continue
			}

			err = cfg.manageExtendedLabels(&mf, labelKeyNames, out)
			if err != nil {
				return &VerifyError{"Failed to extend labels on metrics from proxied server", err}

			}

		}

		// Prometheus always request with "Accept-Encoding: gzip"
		if resp.Request.Header.Get("Accept-Encoding") == "gzip" {
			var b bytes.Buffer
			gz := gzip.NewWriter(&b)
			defer gz.Close()
			_, err = gz.Write(out.Bytes())
			gz.Flush()
			resp.Body = io.NopCloser(&b)
		} else {
			resp.Body = io.NopCloser(out)
		}
		return nil
	}
}

func countBoundRules(boundLabels []boundExtendedLabel) int {
	count := 0
	for _, bound := range boundLabels {
		count += len(bound.matchLabels)
	}
	return count
}

// manageExtendedLabels applies the extended label rules to the given metric family and writes the modified metrics to
// the output buffer.
func (cfg moduleConfig) manageExtendedLabels(mf *dto.MetricFamily, labelKeyNames *ExtendedLabelTarget, out *bytes.Buffer) error {
	// Bind and filter rules once per metric family, then apply to each metric.
	boundLabels := bindExtendedLabelsForFamily(
		mf.Metric,
		labelKeyNames,
		mf.GetName(),
	)
	if countBoundRules(boundLabels) <= 1 {
		// Fast path: existing implementation.
		for _, metric := range mf.Metric {
			applyBoundLabelsToMetricSequential(metric, boundLabels)
		}
	} else {
		// Optimized path: reserve capacity once per metric.
		var pending [][]*dto.LabelPair

		for _, metric := range mf.Metric {
			applyBoundLabelsToMetricBatched(metric, boundLabels, &pending)
		}
	}
	/*
			for _, metric := range mf.Metric {
				applyBoundLabelsToMetric(metric, boundLabels)
			}

		var pending [][]*dto.LabelPair

		for _, metric := range mf.Metric {
			applyBoundLabelsToMetric(metric, boundLabels, &pending)
		}
	*/
	// Serialize the complete metric family.
	_, err := expfmt.MetricFamilyToText(out, mf)
	if err != nil {
		proxyMalformedCount.WithLabelValues(cfg.name).Inc()
		return &VerifyError{
			"Failed to inject labels from proxied server",
			err,
		}
	}

	return nil
}

// bindExtendedLabelsForFamily binds the extended label rules to the metrics in the given metric family,
// returning a slice of boundExtendedLabel structs that can be applied to each metric.
func bindExtendedLabelsForFamily(metrics []*dto.Metric, labelKeyNames *ExtendedLabelTarget, metricName string) []boundExtendedLabel {
	firstMetricWithLabels := findFirstMetricWithLabels(metrics)

	if firstMetricWithLabels == nil {
		return nil
	}

	labelIndexMap := resolveLabels(firstMetricWithLabels, labelKeyNames)
	boundLabels := make([]boundExtendedLabel, 0, len(labelKeyNames.Labels))

	for _, extendedLabel := range labelKeyNames.Labels {
		meta, found := labelIndexMap[extendedLabel.MatchLabelKey]
		if !found || !meta.LabelExist {
			continue
		}

		matchingRules := filterRulesForMetricName(metricName, extendedLabel.MatchLabels)
		if len(matchingRules) == 0 {
			continue
		}

		boundLabels = append(boundLabels, boundExtendedLabel{
			labelIndex:  meta.LabelIndex,
			matchLabels: matchingRules,
		})
	}

	return boundLabels
}

// findFirstMetricWithLabels returns the first metric in the given slice that has at least one label, or nil if none do.
func findFirstMetricWithLabels(metrics []*dto.Metric) *dto.Metric {
	for _, metric := range metrics {
		if metric.Label != nil {
			return metric
		}
	}
	return nil
}

// filterRulesForMetricName filters the given slice of MatchLabel rules to only those that match the given metric name.
func filterRulesForMetricName(metricName string, rules []MatchLabel) []MatchLabel {
	matchingRules := make([]MatchLabel, 0, len(rules))
	for _, matchLabel := range rules {
		if matchMetricsName(metricName, &matchLabel) {
			matchingRules = append(matchingRules, matchLabel)
		}
	}
	return matchingRules
}

func applyBoundLabelsToMetricSequential(
	metric *dto.Metric,
	boundLabels []boundExtendedLabel,
) {
	for _, bound := range boundLabels {
		// Guard against different label slice lengths.
		if bound.labelIndex >= len(metric.Label) {
			continue
		}

		matchLabelPair := metric.Label[bound.labelIndex]
		if matchLabelPair == nil {
			continue
		}

		metricLabelValue := matchLabelPair.GetValue()

		for _, matchLabel := range bound.matchLabels {
			applyMatchLabelRules(metric, metricLabelValue, matchLabel)
		}
	}
}

// applyBoundLabelsToMetricBatched applies the bound extended label rules to the given metric, modifying its labels in place.
func applyBoundLabelsToMetricBatched(
	metric *dto.Metric,
	boundLabels []boundExtendedLabel,
	pending *[][]*dto.LabelPair,
) {
	// Reuse the temporary slice from the previous metric.
	additions := (*pending)[:0]
	totalNeeded := 0

	for _, bound := range boundLabels {
		if bound.labelIndex >= len(metric.Label) {
			continue
		}

		matchLabelPair := metric.Label[bound.labelIndex]
		if matchLabelPair == nil {
			continue
		}

		metricLabelValue := matchLabelPair.GetValue()

		for i := range bound.matchLabels {
			rule := &bound.matchLabels[i]

			var exactLabels []*dto.LabelPair
			var wildcardLabels []*dto.LabelPair
			var defaultLabels []*dto.LabelPair

			matchedAnyRule := false

			if pair, found := rule.MatchLabelValues[metricLabelValue]; found {
				matchedAnyRule = true
				if pair != nil {
					exactLabels = pair.compiledLabelPairs
				}
			}

			if pair, found := rule.MatchLabelValues["*"]; found {
				matchedAnyRule = true
				if pair != nil {
					wildcardLabels = pair.compiledLabelPairs
				}
			}

			if matchedAnyRule {
				defaultLabels = rule.compiledDefaultLabelPairs
			}

			// Preserve existing append order:
			// exact, wildcard, defaults, then next rule.
			if len(exactLabels) > 0 {
				additions = append(additions, exactLabels)
				totalNeeded += len(exactLabels)
			}
			if len(wildcardLabels) > 0 {
				additions = append(additions, wildcardLabels)
				totalNeeded += len(wildcardLabels)
			}
			if len(defaultLabels) > 0 {
				additions = append(additions, defaultLabels)
				totalNeeded += len(defaultLabels)
			}
		}
	}

	// Retain any grown scratch capacity for the next metric.
	*pending = additions

	if totalNeeded == 0 {
		return
	}

	// Reserve capacity once for all matching rules.
	if cap(metric.Label)-len(metric.Label) < totalNeeded {
		grown := make(
			[]*dto.LabelPair,
			len(metric.Label),
			len(metric.Label)+totalNeeded,
		)
		copy(grown, metric.Label)
		metric.Label = grown
	}

	for _, labels := range additions {
		metric.Label = append(metric.Label, labels...)
	}
}

// applyMatchLabelRules applies the label extension rules for a specific metric label value, adding any matching labels
// to the metric.

func applyMatchLabelRules(
	metric *dto.Metric,
	metricLabelValue string,
	matchLabel MatchLabel,
) {
	var exactLabels []*dto.LabelPair
	var wildcardLabels []*dto.LabelPair
	var defaultLabels []*dto.LabelPair

	matchedAnyRule := false

	// Exact-value match.
	if addLabels, found := matchLabel.MatchLabelValues[metricLabelValue]; found {
		matchedAnyRule = true

		if addLabels != nil {
			exactLabels = addLabels.compiledLabelPairs
		}
	}

	// Wildcard match is independent of the exact match.
	if addLabels, found := matchLabel.MatchLabelValues["*"]; found {
		matchedAnyRule = true

		if addLabels != nil {
			wildcardLabels = addLabels.compiledLabelPairs
		}
	}

	// Defaults are added once if either rule matched.
	if matchedAnyRule {
		defaultLabels = matchLabel.compiledDefaultLabelPairs
	}

	// Calculate total capacity needed.
	needed := len(exactLabels) +
		len(wildcardLabels) +
		len(defaultLabels)

	if needed == 0 {
		return
	}

	// Reserve capacity once for all three groups.
	if cap(metric.Label)-len(metric.Label) < needed {
		grown := make(
			[]*dto.LabelPair,
			len(metric.Label),
			len(metric.Label)+needed,
		)

		copy(grown, metric.Label)
		metric.Label = grown
	}

	// Append references to the compiled label pairs.
	addExtendedLabels(exactLabels, metric)
	addExtendedLabels(wildcardLabels, metric)
	addExtendedLabels(defaultLabels, metric)
}

//func applyMatchLabelRulesX(metric *dto.Metric, metricLabelValue string, matchLabel MatchLabel) {
//	defaultAddLabels := matchLabel.DefaultLabelPairs
//	matchedAnyRule := false
//
//	// Exact-value match using O(1) map lookup.
//	if addLabels, found := matchLabel.MatchLabelValues[metricLabelValue]; found {
//		matchedAnyRule = true
//		if addLabels != nil {
//			addExtendedLabels(addLabels.LabelPair, metric)
//		}
//	}
//
//	// Wildcard is independent of the exact match.
//	if addLabels, found := matchLabel.MatchLabelValues["*"]; found {
//		matchedAnyRule = true
//		if addLabels != nil {
//			addExtendedLabels(addLabels.LabelPair, metric)
//		}
//	}
//
//	// Add defaults once if any rule matched.
//	if matchedAnyRule && defaultAddLabels != nil {
//		addExtendedLabels(defaultAddLabels, metric)
//	}
//}

// addExtendedLabels appends the given label pairs to the metric's labels, reserving space in advance to avoid repeated
// reallocations.
func addExtendedLabels(
	labels []*dto.LabelPair,
	metric *dto.Metric,
) {
	metric.Label = append(metric.Label, labels...)
}

// matchMetricsName checks if the given metric name matches the MetricMatch pattern in the MatchLabel configuration.
func matchMetricsName(name string, matchLabel *MatchLabel) bool {
	if matchLabel == nil || matchLabel.MetricMatch == nil {
		return true
	}

	if matchLabel.metricMatchRegexp != nil {
		return matchLabel.metricMatchRegexp.MatchString(name)
	}

	// Fallback for manually-constructed MatchLabel values that were not passed
	// through config-time regex precompilation.
	match, err := regexp.MatchString(*matchLabel.MetricMatch, name)
	if err != nil {
		// log
		return false
	}
	return match
}

// getTargetMatch returns the ExtendedLabelTarget configuration for the target specified in the request URL query parameters.
func getTargetMatch(request *http.Request, cfg moduleConfig) *ExtendedLabelTarget {
	target := request.URL.Query().Get(*cfg.HTTP.ExtendLabelsTargetURLIdentity)
	targetConfig := cfg.HTTP.LabelExtendConfig.Targets[target]
	return &targetConfig
}

func resolveLabels(metric *dto.Metric, ingestor *ExtendedLabelTarget) map[string]*MetricLabelMeta {
	metricLabelMeta := make(map[string]*MetricLabelMeta)
	for _, key := range ingestor.getMatchKeys() {
		metricLabel := MetricLabelMeta{
			LabelIndex: 0,
			LabelExist: false,
		}
		metricLabelMeta[key] = &metricLabel
		for index, label := range metric.Label {
			if key == label.GetName() {
				metricLabel.LabelExist = true
				metricLabel.LabelIndex = index
				break
			}
		}
	}
	return metricLabelMeta
}

type MetricLabelMeta struct {
	LabelIndex int
	LabelExist bool
}

type boundExtendedLabel struct {
	labelIndex  int
	matchLabels []MatchLabel
}

func (cfg moduleConfig) getReverseProxyErrorHandlerFunc() func(http.ResponseWriter, *http.Request, error) {
	return func(w http.ResponseWriter, _ *http.Request, err error) {
		var verifyError *VerifyError
		if errors.As(err, &verifyError) {
			log.Errorf("Verification for module '%s' failed: %v", cfg.name, err)
			http.Error(w, VerificationErrorMsg, http.StatusInternalServerError)
			return
		}

		if errors.Is(err, context.DeadlineExceeded) {
			log.Errorf("Request time out for module '%s'", cfg.name)
			http.Error(w, http.StatusText(http.StatusGatewayTimeout), http.StatusGatewayTimeout)
			return
		}

		log.Errorf("Proxy error for module '%s': %v", cfg.name, err)
		http.Error(w, http.StatusText(http.StatusBadGateway), http.StatusBadGateway)
	}
}

// BearerAuthMiddleware checks an Authorization: Berarer header for a known
// token
type BearerAuthMiddleware struct {
	http.Handler
	Token string
}

func (b BearerAuthMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("Authorization header is missing"))
		return
	}
	ss := strings.SplitN(authHeader, " ", 2)
	if !(len(ss) == 2 && ss[0] == "Bearer") {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("Authorization header not of Bearer type"))
		return
	}
	if ss[1] != b.Token {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("Invalid Bearer Token"))
		return
	}
	b.Handler.ServeHTTP(w, r)
}

// IPAddressAuthMiddleware matches all incoming requests to a known
// set of remote net.IPNet networks
type IPAddressAuthMiddleware struct {
	http.Handler
	ACL []net.IPNet
}

func (m IPAddressAuthMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		log.Errorf("Failed to parse host form remote address '%s'", r.RemoteAddr)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("Failed to determine client IP address"))
		return
	}

	addr := net.ParseIP(host)
	if addr == nil {
		log.Errorf(
			"Failed to determine client IP address from '%s' (originally '%s')",
			host, r.RemoteAddr,
		)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("Failed to determine client IP address"))
		return
	}

	for _, network := range m.ACL {
		// client is in access list
		if network.Contains(addr) {
			m.Handler.ServeHTTP(w, r)
			return
		}
	}

	// client is not in access list
	log.Infof("Access forbidden for %q", addr)
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte("Forbidden"))
}
