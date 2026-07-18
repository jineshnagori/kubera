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

package recommender

import (
	"testing"
	"time"
)

var key = ContainerKey{Namespace: "production", Workload: "api", Container: "app"}

func TestRecommendWithoutSamples(t *testing.T) {
	r := New()
	if _, ok := r.Recommend(key, 0.9, 0.95); ok {
		t.Error("expected ok=false with no samples")
	}
}

func TestObserveAndRecommend(t *testing.T) {
	r := New()
	for i := range 20 {
		ts := t0.Add(time.Duration(i) * 30 * time.Second)
		r.Observe(key, "pod-a", 120, 400*1024*1024, ts)
	}

	rec, ok := r.Recommend(key, 0.9, 0.95)
	if !ok {
		t.Fatal("expected a recommendation")
	}
	if rec.CPUMilli < 115 || rec.CPUMilli > 135 {
		t.Errorf("cpu = %v, want ~120-130 (one bucket above 120m)", rec.CPUMilli)
	}
	wantMem := 400.0 * 1024 * 1024
	if rec.MemBytes < wantMem || rec.MemBytes > wantMem*1.1 {
		t.Errorf("mem = %v, want ~%v", rec.MemBytes, wantMem)
	}
}

func TestObserveDeduplicatesSameSample(t *testing.T) {
	r := New()
	// Same pod, same timestamp, observed 10 times (reconcile storm): must
	// count once. A second pod at a higher value observed once should then
	// dominate the p99.
	for range 10 {
		r.Observe(key, "pod-a", 100, 1e8, t0)
	}
	r.Observe(key, "pod-b", 800, 1e8, t0)

	rec, _ := r.Recommend(key, 0.99, 0.95)
	if rec.CPUMilli < 700 {
		t.Errorf("cpu p99 = %v, want ~800: duplicate samples were not dropped", rec.CPUMilli)
	}
}

func TestForget(t *testing.T) {
	r := New()
	r.Observe(key, "pod-a", 100, 1e8, t0)
	r.Forget("production", "api")
	if _, ok := r.Recommend(key, 0.9, 0.95); ok {
		t.Error("expected state dropped after Forget")
	}
}
