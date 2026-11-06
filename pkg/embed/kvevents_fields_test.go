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
	"reflect"
	"testing"

	contract "github.com/inference-sim/blis-substrate/contracts/embed"

	"github.com/llm-d/llm-d-router/pkg/kvevents"
)

// TestEventBatchCarriesTheWidenedFields: every optional field of the contract's events reaches
// llm-d's event, an absent one stays absent, a clear keeps its tier, and a cache salt becomes the
// first block's extra key as llm-d's SGLang adapter produces it.
func TestEventBatchCarriesTheWidenedFields(t *testing.T) {
	id, g, w := 4, 2, 128
	got, err := eventBatch(contract.KVBatch{Events: []contract.KVEvent{
		contract.BlockStored{Hashes: []uint64{1}, BlockSize: 16, LoRAID: &id, LoRAName: "ad",
			ExtraKeys: [][]any{{"ad", "mm"}}, GroupIdx: &g, SpecKind: contract.SpecSlidingWindow, SlidingWindow: &w},
		contract.BlockStored{Hashes: []uint64{2, 3}, BlockSize: 16, CacheSalt: "s"},
		contract.BlockStored{Hashes: []uint64{4}, Parent: 3, BlockSize: 16, CacheSalt: "s"},
		contract.BlockRemoved{Hashes: []uint64{1}, GroupIdx: &g},
		contract.AllBlocksCleared{Tier: "cpu"},
		&contract.AllBlocksCleared{},
	}})
	if err != nil {
		t.Fatal(err)
	}
	name := "ad"
	want := []kvevents.GenericEvent{
		&kvevents.BlockStoredEvent{BlockHashes: []uint64{1}, BlockSize: 16, LoraID: &id, LoraName: &name,
			ExtraKeys: [][]any{{"ad", "mm"}}, GroupIdx: &g, KVCacheSpecKind: kvevents.KVCacheSpecKindSlidingWindow,
			KVCacheSpecSlidingWindowSize: &w},
		&kvevents.BlockStoredEvent{BlockHashes: []uint64{2, 3}, BlockSize: 16, ExtraKeys: [][]any{{"s"}, nil}},
		&kvevents.BlockStoredEvent{BlockHashes: []uint64{4}, ParentHash: 3, BlockSize: 16},
		&kvevents.BlockRemovedEvent{BlockHashes: []uint64{1}, GroupIdx: &g},
		&kvevents.AllBlocksClearedEvent{DeviceTier: "cpu"},
		&kvevents.AllBlocksClearedEvent{},
	}
	if !reflect.DeepEqual(got.Events, want) {
		t.Errorf("events =\n  %#v\nwant\n  %#v", got.Events, want)
	}
}
