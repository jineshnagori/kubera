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

// Package recommender turns usage samples into per-container resource
// recommendations using exponentially-bucketed, time-decaying histograms —
// the same shape as the VPA recommender, in a compact form.
package recommender

import (
	"math"
	"time"
)

// decayingHistogram is a histogram with exponentially growing bucket bounds
// (each bucket ~ratio× the previous) and exponential time decay: a sample's
// effective weight halves every halfLife. Decay is implemented by growing the
// weight of NEW samples (weight = 2^(age since reference/halfLife)) instead of
// rewriting old buckets; weights are rescaled when they grow too large.
type decayingHistogram struct {
	base     float64 // upper bound of bucket 0
	ratio    float64 // bucket growth factor, e.g. 1.05
	halfLife time.Duration

	weights     []float64
	totalWeight float64
	refTime     time.Time // reference for the growing-weight scheme
}

const maxWeightBeforeRescale = 1e12

func newDecayingHistogram(base float64, halfLife time.Duration) *decayingHistogram {
	return &decayingHistogram{
		base:     base,
		ratio:    bucketRatio,
		halfLife: halfLife,
		weights:  make([]float64, bucketCount),
	}
}

func (h *decayingHistogram) bucketFor(value float64) int {
	if value <= h.base {
		return 0
	}
	idx := int(math.Ceil(math.Log(value/h.base) / math.Log(h.ratio)))
	if idx >= len(h.weights) {
		return len(h.weights) - 1
	}
	return idx
}

// upperBound is the value a bucket index represents (conservative: the top of
// the bucket).
func (h *decayingHistogram) upperBound(idx int) float64 {
	return h.base * math.Pow(h.ratio, float64(idx))
}

func (h *decayingHistogram) observe(value float64, t time.Time) {
	if h.refTime.IsZero() {
		h.refTime = t
	}
	weight := math.Exp2(t.Sub(h.refTime).Seconds() / h.halfLife.Seconds())
	if weight > maxWeightBeforeRescale {
		h.rescale(t)
		weight = 1
	}
	h.weights[h.bucketFor(value)] += weight
	h.totalWeight += weight
}

// rescale shifts the reference time forward so future weights start at 1
// again, shrinking all stored weights by the same factor. Relative
// proportions — and therefore percentiles — are unchanged.
func (h *decayingHistogram) rescale(now time.Time) {
	factor := math.Exp2(now.Sub(h.refTime).Seconds() / h.halfLife.Seconds())
	for i := range h.weights {
		h.weights[i] /= factor
	}
	h.totalWeight /= factor
	h.refTime = now
}

func (h *decayingHistogram) empty() bool {
	return h.totalWeight < 1e-9
}

// percentile returns the upper bound of the smallest bucket such that the
// cumulative weight reaches p (0 < p <= 1) of the total.
func (h *decayingHistogram) percentile(p float64) float64 {
	if h.empty() {
		return 0
	}
	threshold := p * h.totalWeight
	cumulative := 0.0
	for i, w := range h.weights {
		cumulative += w
		if cumulative >= threshold {
			return h.upperBound(i)
		}
	}
	return h.upperBound(len(h.weights) - 1)
}
