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

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestHistogramPercentile(t *testing.T) {
	h := newDecayingHistogram(10, 12*time.Hour)

	// 90 samples at ~100m, 10 samples at ~500m.
	for range 90 {
		h.observe(100, t0)
	}
	for range 10 {
		h.observe(500, t0)
	}

	p50 := h.percentile(0.50)
	if p50 < 95 || p50 > 110 {
		t.Errorf("p50 = %v, want ~100", p50)
	}
	p99 := h.percentile(0.99)
	if p99 < 475 || p99 > 530 {
		t.Errorf("p99 = %v, want ~500", p99)
	}
}

func TestHistogramDecayFadesOldSamples(t *testing.T) {
	halfLife := time.Hour
	h := newDecayingHistogram(10, halfLife)

	// Old high plateau, then a long stretch of low usage.
	for range 100 {
		h.observe(1000, t0)
	}
	late := t0.Add(10 * time.Hour) // 10 half-lives: old weight ~1/1024 of new
	for range 100 {
		h.observe(100, late)
	}

	p90 := h.percentile(0.90)
	if p90 > 150 {
		t.Errorf("p90 = %v after decay, want ~100 (old 1000m samples should have faded)", p90)
	}
}

func TestHistogramRescaleKeepsPercentiles(t *testing.T) {
	halfLife := time.Minute
	h := newDecayingHistogram(10, halfLife)

	// Push weights past the rescale threshold: 2^40 > 1e12 at 40 half-lives.
	h.observe(100, t0)
	h.observe(100, t0.Add(45*time.Minute))
	h.observe(100, t0.Add(46*time.Minute))

	p90 := h.percentile(0.90)
	if p90 < 95 || p90 > 110 {
		t.Errorf("p90 = %v after rescale, want ~100", p90)
	}
	if h.totalWeight > maxWeightBeforeRescale {
		t.Errorf("totalWeight = %v, rescale did not shrink weights", h.totalWeight)
	}
}

func TestHistogramValuesBelowBaseLandInFirstBucket(t *testing.T) {
	h := newDecayingHistogram(10, 12*time.Hour)
	h.observe(1, t0)
	if got := h.percentile(1.0); got != 10 {
		t.Errorf("percentile = %v, want base bucket upper bound 10", got)
	}
}

func TestHistogramEmpty(t *testing.T) {
	h := newDecayingHistogram(10, 12*time.Hour)
	if !h.empty() {
		t.Error("new histogram should be empty")
	}
	if got := h.percentile(0.9); got != 0 {
		t.Errorf("percentile of empty histogram = %v, want 0", got)
	}
}
