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
	"sync"

	"k8s.io/apimachinery/pkg/labels"
)

// FakeProvider serves canned usage samples in tests. Samples are returned for
// every selector; namespace scoping is the caller's concern.
type FakeProvider struct {
	mu      sync.Mutex
	usages  map[string][]PodUsage // keyed by namespace
	listErr error
}

var _ Provider = (*FakeProvider)(nil)

func NewFakeProvider() *FakeProvider {
	return &FakeProvider{usages: map[string][]PodUsage{}}
}

// SetPodUsage replaces the samples returned for a namespace.
func (f *FakeProvider) SetPodUsage(namespace string, usages ...PodUsage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.usages[namespace] = usages
}

// SetError makes every ListPodUsage call fail with err (nil to clear).
func (f *FakeProvider) SetError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listErr = err
}

func (f *FakeProvider) ListPodUsage(_ context.Context, namespace string, _ labels.Selector) ([]PodUsage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.usages[namespace], nil
}
