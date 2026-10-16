/*
Copyright 2026 The llm-d Authors.

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

package registry

import (
	"slices"

	"github.com/llm-d/llm-d-router/pkg/common/hosting"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/interface/flowcontrol"
)

// WithRuntime runs the registry on rt: its clock, and, for a hosted runtime, queue iteration in
// flow-ID order instead of sync.Map order.
func WithRuntime(rt hosting.Runtime) RegistryOption {
	return func(fr *FlowRegistry) {
		fr.clock = rt.Clock()
		fr.hosted = rt.Hosted()
	}
}

func (a *priorityBandAccessor) iterateQueuesInOrder(callback func(queue flowcontrol.FlowQueueAccessor) bool) {
	var ids []string
	queues := map[string]*managedQueue{}
	a.band.activeQueues.Range(func(k, v any) bool {
		id := k.(string)
		ids = append(ids, id)
		queues[id] = v.(*managedQueue)
		return true
	})
	slices.Sort(ids)
	for _, id := range ids {
		if !callback(queues[id].FlowQueueAccessor()) {
			return
		}
	}
}
