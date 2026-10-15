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

package kvblock_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-router/pkg/kvcache/kvblock"
)

// PodEngineKeys reports a pod's confirmed engine keys through the traced and instrumented
// decorators, excludes speculative entries and other pods, and follows evictions.
func TestPodEngineKeys(t *testing.T) {
	ctx := context.Background()
	mem, err := kvblock.NewInMemoryIndex(kvblock.DefaultInMemoryIndexConfig())
	require.NoError(t, err)
	idx := kvblock.NewTracedIndex(kvblock.NewInstrumentedIndex(mem))

	podA := kvblock.PodEntry{PodIdentifier: "a:1", DeviceTier: "gpu"}
	podACPU := kvblock.PodEntry{PodIdentifier: "a:1", DeviceTier: "cpu"}
	podB := kvblock.PodEntry{PodIdentifier: "b:1", DeviceTier: "gpu"}
	require.NoError(t, idx.Add(ctx, []kvblock.BlockHash{1, 2}, []kvblock.BlockHash{101, 102}, []kvblock.PodEntry{podA}))
	require.NoError(t, idx.Add(ctx, []kvblock.BlockHash{3}, []kvblock.BlockHash{103}, []kvblock.PodEntry{podACPU}))
	require.NoError(t, idx.Add(ctx, []kvblock.BlockHash{4}, []kvblock.BlockHash{104}, []kvblock.PodEntry{podB}))
	require.NoError(t, idx.Add(ctx, nil, []kvblock.BlockHash{104},
		[]kvblock.PodEntry{{PodIdentifier: "a:1", DeviceTier: "gpu", Speculative: true}}))

	got, err := kvblock.PodEngineKeys(idx, "a:1")
	require.NoError(t, err)
	assert.Equal(t, map[uint64]bool{1: true, 2: true, 3: true}, got)

	require.NoError(t, idx.Evict(ctx, kvblock.EngineKey, []kvblock.BlockHash{2}, []kvblock.PodEntry{podA}))
	got, err = kvblock.PodEngineKeys(idx, "a:1")
	require.NoError(t, err)
	assert.Equal(t, map[uint64]bool{1: true, 3: true}, got)

	got, err = kvblock.PodEngineKeys(idx, "b:1")
	require.NoError(t, err)
	assert.Equal(t, map[uint64]bool{4: true}, got)
}

func TestPodEngineKeys_UnsupportedIndex(t *testing.T) {
	_, err := kvblock.PodEngineKeys(nil, "a:1")
	require.Error(t, err)
}
