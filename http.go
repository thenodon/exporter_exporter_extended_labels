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

// aha start
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

func (cfg moduleConfig) manageExtendedLabels(mf *dto.MetricFamily, labelKeyNames *ExtendedLabelTarget, out *bytes.Buffer) error {

	var labelIndexMap map[string]*MetricLabelMeta
	// Build once per metric family: each entry binds a configured label rule to
	// the resolved label index from the first usable metric, so the hot loop can
	// avoid repeated map lookups by MatchLabelKey.
	boundLabels := make([]boundExtendedLabel, 0, len(labelKeyNames.Labels))

	for _, metric := range mf.Metric {
		// Delay binding until we find a metric that actually has labels.
		// This avoids bailing out early when the first metric has nil labels.
		if labelIndexMap == nil {
			if metric.Label == nil {
				continue
			}

			// return the label map indicate if the label_key_names exist in the metrics
			labelIndexMap = resolveLabels(metric, labelKeyNames)
			for _, extendedLabel := range labelKeyNames.Labels {
				meta, found := labelIndexMap[extendedLabel.MatchLabelKey]
				if !found || !meta.LabelExist {
					continue
				}
				boundLabels = append(boundLabels, boundExtendedLabel{
					labelIndex:  meta.LabelIndex,
					matchLabels: extendedLabel.MatchLabels,
				})
			}
		}

		// For every pre-bound label rule, apply matching against the current metric.
		for _, bound := range boundLabels {
			// Label indexes are resolved from the first metric of the family.
			// Guarding the index avoids panics if later metrics have different label shapes.
			if bound.labelIndex >= len(metric.Label) {
				continue
			}
			matchLabelPair := metric.Label[bound.labelIndex]
			if matchLabelPair != nil {
				for _, matchLabel := range bound.matchLabels {
					defaultAddLabels := matchLabel.DefaultLabelPairs

					// check if metric name matcher is true
					if !matchMetricsName(mf.GetName(), &matchLabel) {
						continue
					}

					metricLabelValue := matchLabelPair.GetValue()
					matchedAnyRule := false

					// Fast path: apply exact-value rule in O(1) via map lookup.
					if addLabels, found := matchLabel.MatchLabelValues[metricLabelValue]; found {
						matchedAnyRule = true
						if addLabels != nil {
							addExtendedLabels(addLabels.LabelPair, metric)
						}
					}

					// Wildcard rule is an independent match and should still be
					// evaluated even if the exact-value rule matched.
					if addLabels, found := matchLabel.MatchLabelValues["*"]; found {
						matchedAnyRule = true
						if addLabels != nil {
							addExtendedLabels(addLabels.LabelPair, metric)
						}
					}

					// Default labels are tied to this MatchLabel and should be
					// appended at most once when any rule (exact or wildcard) matched.
					if matchedAnyRule && defaultAddLabels != nil {
						addExtendedLabels(defaultAddLabels, metric)
					}
				}
			}
		}
	}

	_, err := expfmt.MetricFamilyToText(out, mf)
	if err != nil {
		if err != nil {
			proxyMalformedCount.WithLabelValues(cfg.name).Inc()
			return &VerifyError{"Failed to inject labels from proxied server", err}
		}
	}
	return nil
}

func addExtendedLabels(defaultAddLabels map[string]string, metric *dto.Metric) {
	// Reserve space once for this whole batch of labels to avoid repeated
	// reallocations while appending one label at a time.
	needed := len(defaultAddLabels)
	if needed == 0 {
		return
	}
	if available := cap(metric.Label) - len(metric.Label); available < needed {
		grown := make([]*dto.LabelPair, len(metric.Label), len(metric.Label)+needed)
		copy(grown, metric.Label)
		metric.Label = grown
	}

	for addLabelKey, addLabelValue := range defaultAddLabels {
		k := addLabelKey
		v := addLabelValue
		var label = dto.LabelPair{
			Name:  &k,
			Value: &v,
		}
		metric.Label = append(metric.Label, &label)
	}
}

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
