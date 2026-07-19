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

package actuator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ErrRateLimited is returned by Apply when the cluster-wide resize budget is
// exhausted; the caller should skip and retry on the next poll tick rather
// than treat it as a failure.
var ErrRateLimited = errors.New("cluster-wide resize rate limit reached")

// Resizer applies PodPlans through the Pod "resize" subresource (Kubernetes
// 1.33+). A plain Pod PATCH of spec resources is rejected there; the
// subresource is the only supported path. Limiter (optional) throttles
// resizes cluster-wide to prevent thundering herds when many
// DynamicResources act at once.
type Resizer struct {
	Client  client.Client
	Limiter *rate.Limiter
}

func (r *Resizer) Apply(ctx context.Context, namespace string, plan *PodPlan) error {
	if r.Limiter != nil && !r.Limiter.Allow() {
		return ErrRateLimited
	}
	type containerPatch struct {
		Name      string                      `json:"name"`
		Resources corev1.ResourceRequirements `json:"resources"`
	}
	containers := make([]containerPatch, 0, len(plan.Changes))
	for _, ch := range plan.Changes {
		containers = append(containers, containerPatch{
			Name: ch.Name,
			Resources: corev1.ResourceRequirements{
				Requests: ch.Requests,
				Limits:   ch.Limits,
			},
		})
	}
	patch, err := json.Marshal(map[string]any{
		"spec": map[string]any{"containers": containers},
	})
	if err != nil {
		return err
	}

	pod := &corev1.Pod{}
	pod.Name = plan.Pod
	pod.Namespace = namespace
	if err := r.Client.SubResource("resize").Patch(ctx, pod,
		client.RawPatch(types.StrategicMergePatchType, patch)); err != nil {
		return fmt.Errorf("resizing pod %s: %w", plan.Pod, err)
	}
	return nil
}

// DetectInPlaceResize reports whether the cluster supports the Pod resize
// subresource (kube-apiserver 1.33+, InPlacePodVerticalScaling on by
// default).
func DetectInPlaceResize(dc discovery.DiscoveryInterface) (bool, error) {
	v, err := dc.ServerVersion()
	if err != nil {
		return false, err
	}
	major, err := strconv.Atoi(strings.TrimSuffix(v.Major, "+"))
	if err != nil {
		return false, fmt.Errorf("parsing server major version %q: %w", v.Major, err)
	}
	minor, err := strconv.Atoi(strings.TrimSuffix(v.Minor, "+"))
	if err != nil {
		return false, fmt.Errorf("parsing server minor version %q: %w", v.Minor, err)
	}
	return major > 1 || (major == 1 && minor >= 33), nil
}

// CooldownTracker enforces per-workload, per-direction minimum intervals
// between resizes. In-memory: a controller restart resets cooldowns, which is
// safe (worst case one early resize).
type CooldownTracker struct {
	mu   sync.Mutex
	last map[string]map[Direction]time.Time
}

func NewCooldownTracker() *CooldownTracker {
	return &CooldownTracker{last: map[string]map[Direction]time.Time{}}
}

// Allow reports whether a resize in this direction is outside the cooldown.
func (t *CooldownTracker) Allow(key string, d Direction, cooldown time.Duration, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	last, ok := t.last[key][d]
	return !ok || now.Sub(last) >= cooldown
}

// Record stamps a resize in this direction.
func (t *CooldownTracker) Record(key string, d Direction, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.last[key] == nil {
		t.last[key] = map[Direction]time.Time{}
	}
	t.last[key][d] = now
}
