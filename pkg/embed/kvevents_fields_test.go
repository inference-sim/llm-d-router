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
	"strings"
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

// Each refusal has a pair (SIM-9): the smallest event that triggers it, and the closest that does
// not.

// TestEventBatchRefusesANegativeGroup: vLLM's decoder refuses a negative group_idx, and -1 is
// the dedup filter's no-group sentinel. Group 0 is a group.
func TestEventBatchRefusesANegativeGroup(t *testing.T) {
	neg, zero := -1, 0
	for name, ev := range map[string]contract.KVEvent{
		"stored":  contract.BlockStored{Hashes: []uint64{1}, BlockSize: 16, GroupIdx: &neg},
		"removed": contract.BlockRemoved{Hashes: []uint64{1}, GroupIdx: &neg},
	} {
		if _, err := eventBatch(contract.KVBatch{Events: []contract.KVEvent{ev}}); err == nil ||
			!strings.Contains(err.Error(), "negative") {
			t.Errorf("%s with group -1: want a negative-group error, got %v", name, err)
		}
	}
	if _, err := eventBatch(contract.KVBatch{Events: []contract.KVEvent{
		contract.BlockStored{Hashes: []uint64{1}, BlockSize: 16, GroupIdx: &zero},
		contract.BlockRemoved{Hashes: []uint64{1}, GroupIdx: &zero}}}); err != nil {
		t.Errorf("group 0: want it accepted, got %v", err)
	}
}

// TestEventBatchRefusesASaltWithExtraKeys: an engine sets one of the two; the salt would be
// dropped silently.
func TestEventBatchRefusesASaltWithExtraKeys(t *testing.T) {
	_, err := eventBatch(contract.KVBatch{Events: []contract.KVEvent{
		contract.BlockStored{Hashes: []uint64{1}, BlockSize: 16, CacheSalt: "s", ExtraKeys: [][]any{{"s"}}}}})
	if err == nil || !strings.Contains(err.Error(), "one of the two") {
		t.Errorf("a salt with extra keys: want the one-of-two error, got %v", err)
	}
	for name, ev := range map[string]contract.BlockStored{
		"salt only":       {Hashes: []uint64{1}, BlockSize: 16, CacheSalt: "s"},
		"extra keys only": {Hashes: []uint64{1}, BlockSize: 16, ExtraKeys: [][]any{{"s"}}},
	} {
		if _, err := eventBatch(contract.KVBatch{Events: []contract.KVEvent{ev}}); err != nil {
			t.Errorf("%s: want it accepted, got %v", name, err)
		}
	}
}

// TestEventBatchRefusesALoRANameItCannotAttribute: llm-d's vLLM adapter reads lora_name and its
// SGLang adapter does not, and the batch does not say the engine. A name beside an id and no
// field only vLLM emits could be either's, so it is refused; the parity test pins the rest.
func TestEventBatchRefusesALoRANameItCannotAttribute(t *testing.T) {
	id, g := 7, 0
	_, err := eventBatch(contract.KVBatch{Events: []contract.KVEvent{
		contract.BlockStored{Hashes: []uint64{1}, BlockSize: 16, LoRAID: &id, LoRAName: "ad"}}})
	if err == nil || !strings.Contains(err.Error(), "lora_name") {
		t.Errorf("a name beside an id, no vLLM-only field: want a refusal, got %v", err)
	}
	for name, ev := range map[string]contract.BlockStored{
		"name with a vLLM-only field": {Hashes: []uint64{1}, BlockSize: 16, LoRAID: &id, LoRAName: "ad", GroupIdx: &g},
		"name alone (vLLM)":           {Hashes: []uint64{1}, BlockSize: 16, LoRAName: "ad"},
		"id alone":                    {Hashes: []uint64{1}, BlockSize: 16, LoRAID: &id},
	} {
		if _, err := eventBatch(contract.KVBatch{Events: []contract.KVEvent{ev}}); err != nil {
			t.Errorf("%s: want it accepted, got %v", name, err)
		}
	}
}
