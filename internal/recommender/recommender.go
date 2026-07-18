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
	"sync"
	"time"
)

// Histogram shape. CPU in millicores from 10m up (~189 buckets to 100 cores),
// memory in bytes from 10Mi up. ratio 1.05 gives ~5% bucket resolution.
const (
	cpuBaseMilli   = 10.0
	memBaseBytes   = 10.0 * 1024 * 1024
	bucketRatio    = 1.05
	bucketCount    = 200
	defaultHalfDay = 12 * time.Hour
)

// ContainerKey identifies one container of one workload.
type ContainerKey struct {
	Namespace string
	Workload  string
	Container string
}

// Recommendation is a percentile readout in raw units.
type Recommendation struct {
	CPUMilli float64
	MemBytes float64
}

type containerState struct {
	cpu *decayingHistogram
	mem *decayingHistogram
	// lastSample deduplicates Metrics Server readings per pod: reconciles can
	// fire faster than the metrics resolution, and re-observing the same
	// sample would skew the histogram.
	lastSample map[string]time.Time
}

// Recommender aggregates usage samples across all pods of a workload into
// per-container histograms. In-memory only: state rebuilds after a restart
// within a few polling intervals.
type Recommender struct {
	mu       sync.Mutex
	halfLife time.Duration
	states   map[ContainerKey]*containerState
}

func New() *Recommender {
	return &Recommender{
		halfLife: defaultHalfDay,
		states:   map[ContainerKey]*containerState{},
	}
}

// Observe feeds one pod-container usage sample. Samples with a timestamp not
// newer than the previous one for the same pod are dropped.
func (r *Recommender) Observe(key ContainerKey, pod string, cpuMilli, memBytes float64, t time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()

	state, ok := r.states[key]
	if !ok {
		state = &containerState{
			cpu:        newDecayingHistogram(cpuBaseMilli, r.halfLife),
			mem:        newDecayingHistogram(memBaseBytes, r.halfLife),
			lastSample: map[string]time.Time{},
		}
		r.states[key] = state
	}

	if last, seen := state.lastSample[pod]; seen && !t.After(last) {
		return
	}
	state.lastSample[pod] = t

	state.cpu.observe(cpuMilli, t)
	state.mem.observe(memBytes, t)
}

// Recommend returns percentile readouts for a container, or ok=false when no
// samples have been observed yet.
func (r *Recommender) Recommend(key ContainerKey, cpuPercentile, memPercentile float64) (Recommendation, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	state, ok := r.states[key]
	if !ok || state.cpu.empty() {
		return Recommendation{}, false
	}
	return Recommendation{
		CPUMilli: state.cpu.percentile(cpuPercentile),
		MemBytes: state.mem.percentile(memPercentile),
	}, true
}

// Forget drops all state for a namespace/workload pair (workload deleted or
// no longer matched).
func (r *Recommender) Forget(namespace, workload string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key := range r.states {
		if key.Namespace == namespace && key.Workload == workload {
			delete(r.states, key)
		}
	}
}
