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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/labels"
)

func promServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		if !strings.Contains(q, `kube_pod_labels{namespace="production",label_app="api"}`) {
			t.Errorf("query missing kube_pod_labels join with sanitized selector: %s", q)
		}
		var result string
		if strings.Contains(q, "container_cpu_usage_seconds_total") {
			result = `{"metric":{"pod":"api-1","container":"app"},"value":[1721390000,"0.120"]}`
		} else {
			result = `{"metric":{"pod":"api-1","container":"app"},"value":[1721390000,"419430400"]}`
		}
		_, _ = fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":[%s]}}`, result)
	}))
}

func TestPrometheusProviderListPodUsage(t *testing.T) {
	srv := promServer(t)
	defer srv.Close()

	p := NewPrometheusProvider(srv.URL)
	sel := labels.SelectorFromSet(labels.Set{"app": "api"})
	usages, err := p.ListPodUsage(context.Background(), "production", sel)
	if err != nil {
		t.Fatal(err)
	}
	if len(usages) != 1 || usages[0].Pod != "api-1" {
		t.Fatalf("usages = %+v, want one sample for api-1", usages)
	}
	cu := usages[0].Containers[0]
	if cu.Container != "app" {
		t.Errorf("container = %q, want app", cu.Container)
	}
	if got := cu.CPU.MilliValue(); got != 120 {
		t.Errorf("cpu = %dm, want 120m (0.120 cores)", got)
	}
	if got := cu.Memory.Value(); got != 419430400 {
		t.Errorf("memory = %d, want 419430400 (400Mi)", got)
	}
}

func TestPrometheusProviderRejectsMatchExpressions(t *testing.T) {
	p := NewPrometheusProvider("http://unused")
	sel, err := labels.Parse("app in (a, b)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.ListPodUsage(context.Background(), "ns", sel); err == nil {
		t.Error("multi-value selectors must be rejected: PromQL join cannot express them")
	}
}

func TestPromLabelName(t *testing.T) {
	if got := promLabelName("app.kubernetes.io/name"); got != "label_app_kubernetes_io_name" {
		t.Errorf("got %q", got)
	}
}
