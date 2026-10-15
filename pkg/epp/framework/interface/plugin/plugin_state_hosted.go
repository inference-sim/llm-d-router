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

package plugin

import "slices"

// rangeLastAccess ranges over requestToLastAccessTime like sync.Map.Range. Under a hosted runtime
// it visits requests in request-ID order, so reaping and eviction callbacks are reproducible;
// otherwise it is sync.Map.Range itself.
func (s *PluginState) rangeLastAccess(f func(k, v any) bool) {
	if !s.rt.Hosted() {
		s.requestToLastAccessTime.Range(f)
		return
	}
	var ids []string
	s.requestToLastAccessTime.Range(func(k, _ any) bool {
		ids = append(ids, k.(string))
		return true
	})
	slices.Sort(ids)
	for _, id := range ids {
		if v, ok := s.requestToLastAccessTime.Load(id); ok && !f(id, v) {
			return
		}
	}
}
