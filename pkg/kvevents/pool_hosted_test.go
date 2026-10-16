// Copyright 2026 The llm-d Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kvevents //nolint:testpackage // asserts the hosted pool has no queues

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/llm-d/llm-d-router/pkg/kvcache/kvblock"
)

func newTestHostedPool(t *testing.T, blockSize int) (*Pool, kvblock.Index, kvblock.TokenProcessor) {
	t.Helper()
	idx, err := kvblock.NewInMemoryIndex(kvblock.DefaultInMemoryIndexConfig())
	require.NoError(t, err)
	tp, err := kvblock.NewChunkedTokenDatabase(&kvblock.TokenProcessorConfig{BlockSizeTokens: blockSize, HashSeed: "test"})
	require.NoError(t, err)
	pool, err := NewHostedPool(DefaultConfig(), idx, tp, nil)
	require.NoError(t, err)
	return pool, idx, tp
}

// A hosted pool has no queue shards and no workers, so nothing runs on the wall clock.
func TestNewHostedPool_NoQueuesNoWorkers(t *testing.T) {
	pool, _, _ := newTestHostedPool(t, 4)
	assert.Empty(t, pool.queues)
	assert.Zero(t, pool.concurrency)
	pool.Shutdown(context.Background()) // nothing to stop; must not block
}

func TestNewHostedPool_RequiresIndexAndTokenProcessor(t *testing.T) {
	idx, err := kvblock.NewInMemoryIndex(kvblock.DefaultInMemoryIndexConfig())
	require.NoError(t, err)
	tp, err := kvblock.NewChunkedTokenDatabase(&kvblock.TokenProcessorConfig{BlockSizeTokens: 4})
	require.NoError(t, err)
	_, err = NewHostedPool(nil, nil, tp, nil)
	require.Error(t, err)
	_, err = NewHostedPool(nil, idx, nil, nil)
	require.Error(t, err)
}

// ProcessEventBatch applies stores, removes and clears synchronously, with the worker path's
// semantics; ClearPod drops only the named pod.
func TestHostedPool_ProcessEventBatch(t *testing.T) {
	ctx := context.Background()
	pool, idx, tp := newTestHostedPool(t, 4)
	tokens := []uint32{1, 2, 3, 4, 5, 6, 7, 8}
	requestKeys, err := tp.TokensToKVBlockKeys(kvblock.EmptyBlockHash, tokens, "m", nil)
	require.NoError(t, err)
	require.Len(t, requestKeys, 2)

	lookup := func(pod string) int {
		got, err := idx.Lookup(ctx, requestKeys, sets.New(pod))
		require.NoError(t, err)
		return len(got)
	}

	store := &EventBatch{Events: []GenericEvent{&BlockStoredEvent{BlockHashes: []uint64{11, 12}, Tokens: tokens, BlockSize: 4}}}
	require.NoError(t, pool.ProcessEventBatch(ctx, store, "pod-a:80", "m"))
	require.NoError(t, pool.ProcessEventBatch(ctx, store, "pod-b:80", "m"))
	assert.Equal(t, 2, lookup("pod-a:80"))
	assert.Equal(t, 2, lookup("pod-b:80"))

	remove := &EventBatch{Events: []GenericEvent{&BlockRemovedEvent{BlockHashes: []uint64{12}}}}
	require.NoError(t, pool.ProcessEventBatch(ctx, remove, "pod-a:80", "m"))
	assert.Equal(t, 1, lookup("pod-a:80"))

	require.NoError(t, pool.ClearPod(ctx, "pod-a:80"))
	assert.Equal(t, 0, lookup("pod-a:80"))
	assert.Equal(t, 2, lookup("pod-b:80"))

	cleared := &EventBatch{Events: []GenericEvent{&AllBlocksClearedEvent{}}}
	require.NoError(t, pool.ProcessEventBatch(ctx, cleared, "pod-b:80", "m"))
	assert.Equal(t, 0, lookup("pod-b:80"))
}

func TestHostedPool_ProcessEventBatchErrors(t *testing.T) {
	pool, _, _ := newTestHostedPool(t, 4)
	require.Error(t, pool.ProcessEventBatch(context.Background(), nil, "p", "m"))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, pool.ProcessEventBatch(ctx, &EventBatch{}, "p", "m"), context.Canceled)
	require.ErrorIs(t, pool.ClearPod(ctx, "p"), context.Canceled)
}
