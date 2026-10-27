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

package embed

import (
	"context"
	"reflect"
	"testing"
	"time"

	contract "github.com/inference-sim/blis-schemas/embed"
	"github.com/inference-sim/blis-schemas/embed/embedtest"

	"github.com/llm-d/llm-d-router/pkg/kvevents"
)

// TestEventBatch: each contract event becomes vLLM's event of the same content, in order, with
// the batch time in float seconds; a CPU-tier store carries no tokens.
func TestEventBatch(t *testing.T) {
	at := embedtest.Epoch.Add(1500 * time.Millisecond)
	got, err := eventBatch(contract.KVBatch{Timestamp: at, Events: []contract.KVEvent{
		contract.BlockStored{Hashes: []uint64{1, 2}, Parent: 9, Tokens: []uint32{1, 2, 3, 4}, BlockSize: 2},
		contract.BlockStored{Hashes: []uint64{3}, BlockSize: 2, Tier: "cpu"},
		&contract.BlockRemoved{Hashes: []uint64{1}, Tier: "cpu"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := &kvevents.EventBatch{Timestamp: float64(at.UnixNano()) / 1e9, Events: []kvevents.GenericEvent{
		&kvevents.BlockStoredEvent{BlockHashes: []uint64{1, 2}, ParentHash: 9, Tokens: []uint32{1, 2, 3, 4}, BlockSize: 2},
		&kvevents.BlockStoredEvent{BlockHashes: []uint64{3}, BlockSize: 2, DeviceTier: "cpu"},
		&kvevents.BlockRemovedEvent{BlockHashes: []uint64{1}, DeviceTier: "cpu"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("eventBatch =\n  %#v\nwant\n  %#v", got, want)
	}
}

// TestKVIndexFollowsEveryTier: the index the precise producer keeps per tier follows the
// batches ingested for it, and a clear drops every tier; a router without the producer neither
// wants events nor reports an index.
func TestKVIndexFollowsEveryTier(t *testing.T) {
	r, err := build(t, poolConfig(kvConfig))
	if err != nil {
		t.Fatal(err)
	}
	if !r.WantsKVEvents() {
		t.Fatal("WantsKVEvents is false with a precise-prefix-cache-producer")
	}
	in := r.(contract.KVIndexInspector)
	const ep = "10.0.0.1:8000"
	tokens := make([]uint32, 32)
	for i := range tokens {
		tokens[i] = uint32(i + 1)
	}
	ingest := func(ev ...contract.KVEvent) {
		t.Helper()
		if err := r.IngestKVEvents(context.Background(), ep, "test-model", contract.KVBatch{Timestamp: embedtest.Epoch, Events: ev}); err != nil {
			t.Fatal(err)
		}
	}
	tier := func(name string) map[uint64]bool {
		t.Helper()
		got, err := in.KVBlocks(ep, name)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	ingest(contract.BlockStored{Hashes: []uint64{11, 12}, Tokens: tokens, BlockSize: 16},
		contract.BlockStored{Hashes: []uint64{11}, BlockSize: 16, Tier: "cpu"})
	if got := tier(""); !reflect.DeepEqual(got, map[uint64]bool{11: true, 12: true}) {
		t.Errorf("every tier: %v", got)
	}
	if got := tier("cpu"); !reflect.DeepEqual(got, map[uint64]bool{11: true}) {
		t.Errorf("cpu tier: %v", got)
	}
	ingest(contract.BlockRemoved{Hashes: []uint64{12}})
	if got := tier("gpu"); !reflect.DeepEqual(got, map[uint64]bool{11: true}) {
		t.Errorf("gpu tier after removing 12: %v", got)
	}
	if err := r.ClearKVEvents(context.Background(), ep); err != nil {
		t.Fatal(err)
	}
	if got := tier(""); len(got) != 0 {
		t.Errorf("after the clear: %v", got)
	}

	plain, err := build(t, poolConfig(routeConfig))
	if err != nil {
		t.Fatal(err)
	}
	if plain.WantsKVEvents() {
		t.Error("WantsKVEvents without a precise-prefix-cache-producer")
	}
	if _, err := plain.(contract.KVIndexInspector).KVBlocks(ep, ""); err == nil {
		t.Error("KVBlocks without a precise-prefix-cache-producer: want an error")
	}
}
