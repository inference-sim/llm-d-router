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

package kvblock

import "fmt"

// PodEngineKeys returns the engine keys idx holds for podIdentifier: each engine key whose mapped
// request keys all carry a confirmed (non-speculative) entry for the pod, on any device tier.
// Speculative entries have no engine keys and never appear. It is an inspection aid for a host
// that embeds the router (see pkg/common/hosting); it reads without refreshing LRU recency, so it
// does not change what the index later evicts. Only the in-memory index, optionally wrapped in the
// traced and instrumented decorators, can be enumerated; any other index returns an error.
func PodEngineKeys(idx Index, podIdentifier string) (map[uint64]bool, error) {
	m, ok := unwrapInMemory(idx)
	if !ok {
		return nil, fmt.Errorf("kvblock: index %T cannot enumerate engine keys", idx)
	}
	out := map[uint64]bool{}
	for _, engineKey := range m.engineToRequestKeys.Keys() {
		requestKeys, found := m.engineToRequestKeys.Peek(engineKey)
		if !found || len(requestKeys) == 0 {
			continue
		}
		if m.podHoldsAll(requestKeys, podIdentifier) {
			out[uint64(engineKey)] = true
		}
	}
	return out, nil
}

// podHoldsAll reports whether every request key carries a confirmed entry for podIdentifier.
func (m *InMemoryIndex) podHoldsAll(requestKeys []BlockHash, podIdentifier string) bool {
	for _, requestKey := range requestKeys {
		podCache, found := m.data.Peek(requestKey)
		if !found || podCache == nil {
			return false
		}
		confirmed := false
		for _, entry := range podCache.matching(podIdentifier) {
			if !entry.Speculative {
				confirmed = true
				break
			}
		}
		if !confirmed {
			return false
		}
	}
	return true
}

// unwrapInMemory strips the traced and instrumented decorators off idx down to an InMemoryIndex.
func unwrapInMemory(idx Index) (*InMemoryIndex, bool) {
	for {
		switch v := idx.(type) {
		case *InMemoryIndex:
			return v, true
		case *tracedIndex:
			idx = v.next
		case *tracedWalker:
			idx = v.next
		case *instrumentedIndex:
			idx = v.next
		case *instrumentedWalker:
			idx = v.next
		default:
			return nil, false
		}
	}
}
