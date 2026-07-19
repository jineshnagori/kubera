/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/labels"
)

// PrometheusProvider reads Pod usage from a Prometheus HTTP API. Pod-to-label
// mapping uses the kube_pod_labels series from kube-state-metrics, so that
// component must be scraped. Only matchLabels selectors are supported —
// matchExpressions cannot be translated to a PromQL vector match.
type PrometheusProvider struct {
	BaseURL string
	Client  *http.Client
}

func NewPrometheusProvider(baseURL string) *PrometheusProvider {
	return &PrometheusProvider{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Client:  &http.Client{Timeout: 15 * time.Second},
	}
}

var promLabelSanitizer = regexp.MustCompile(`[^a-zA-Z0-9_]`)

// kube-state-metrics exposes pod label "app.kubernetes.io/name" as
// label_app_kubernetes_io_name.
func promLabelName(key string) string {
	return "label_" + promLabelSanitizer.ReplaceAllString(key, "_")
}

func (p *PrometheusProvider) selectorMatchers(selector labels.Selector) (string, error) {
	reqs, selectable := selector.Requirements()
	if !selectable {
		return "", fmt.Errorf("selector is not selectable")
	}
	var matchers []string
	for _, req := range reqs {
		if req.Operator() != "=" && req.Operator() != "==" && req.Operator() != "in" {
			return "", fmt.Errorf("prometheus provider supports only matchLabels selectors (got operator %q)", req.Operator())
		}
		vals := req.Values().List()
		if len(vals) != 1 {
			return "", fmt.Errorf("prometheus provider supports only single-value matchers for key %q", req.Key())
		}
		matchers = append(matchers, fmt.Sprintf(`%s=%q`, promLabelName(req.Key()), vals[0]))
	}
	return strings.Join(matchers, ","), nil
}

func (p *PrometheusProvider) ListPodUsage(ctx context.Context, namespace string, selector labels.Selector) ([]PodUsage, error) {
	matchers, err := p.selectorMatchers(selector)
	if err != nil {
		return nil, err
	}

	// kube_pod_labels is a constant 1 per pod, so multiplying filters the
	// usage series down to pods carrying the selector's labels.
	join := fmt.Sprintf(`* on(pod) group_left() kube_pod_labels{namespace=%q,%s}`, namespace, matchers)
	cpuQuery := fmt.Sprintf(
		`sum by (pod, container) (rate(container_cpu_usage_seconds_total{namespace=%q,container!="",container!="POD"}[2m])) %s`,
		namespace, join)
	memQuery := fmt.Sprintf(
		`sum by (pod, container) (container_memory_working_set_bytes{namespace=%q,container!="",container!="POD"}) %s`,
		namespace, join)

	cpu, err := p.query(ctx, cpuQuery)
	if err != nil {
		return nil, fmt.Errorf("prometheus cpu query: %w", err)
	}
	mem, err := p.query(ctx, memQuery)
	if err != nil {
		return nil, fmt.Errorf("prometheus memory query: %w", err)
	}

	now := time.Now()
	byPod := map[string]map[string]*ContainerUsage{}
	ensure := func(pod, container string) *ContainerUsage {
		if byPod[pod] == nil {
			byPod[pod] = map[string]*ContainerUsage{}
		}
		if byPod[pod][container] == nil {
			byPod[pod][container] = &ContainerUsage{Container: container}
		}
		return byPod[pod][container]
	}
	for _, s := range cpu {
		cu := ensure(s.pod, s.container)
		cu.CPU = *resource.NewMilliQuantity(int64(s.value*1000), resource.DecimalSI)
	}
	for _, s := range mem {
		cu := ensure(s.pod, s.container)
		cu.Memory = *resource.NewQuantity(int64(s.value), resource.BinarySI)
	}

	usages := make([]PodUsage, 0, len(byPod))
	for pod, containers := range byPod {
		u := PodUsage{Pod: pod, Timestamp: now}
		for _, cu := range containers {
			u.Containers = append(u.Containers, *cu)
		}
		usages = append(usages, u)
	}
	return usages, nil
}

type promSample struct {
	pod, container string
	value          float64
}

func (p *PrometheusProvider) query(ctx context.Context, q string) ([]promSample, error) {
	reqURL := fmt.Sprintf("%s/api/v1/query?query=%s", p.BaseURL, url.QueryEscape(q))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("prometheus returned %s", resp.Status)
	}

	var body struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Value  []any             `json:"value"` // [timestamp, "value"]
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	if body.Status != "success" {
		return nil, fmt.Errorf("prometheus query status %q", body.Status)
	}

	samples := make([]promSample, 0, len(body.Data.Result))
	for _, r := range body.Data.Result {
		if len(r.Value) != 2 {
			continue
		}
		raw, ok := r.Value[1].(string)
		if !ok {
			continue
		}
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			continue
		}
		samples = append(samples, promSample{
			pod:       r.Metric["pod"],
			container: r.Metric["container"],
			value:     v,
		})
	}
	return samples, nil
}

var _ Provider = (*PrometheusProvider)(nil)
