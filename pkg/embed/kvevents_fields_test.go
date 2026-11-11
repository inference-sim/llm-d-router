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

// TestEventBatchReadsTheLoRANameByEngine: llm-d's vLLM adapter reads lora_name, and the pool takes
// it as the model of the blocks; its SGLang adapter reads lora_id only. A name is carried for
// vLLM (named or unset) and dropped for SGLang, with or without a lora_id; the id is carried for
// both.
func TestEventBatchReadsTheLoRANameByEngine(t *testing.T) {
	id, g := 7, 0
	for _, tc := range []struct {
		engine   string
		ev       contract.BlockStored
		wantName bool
	}{
		{"", contract.BlockStored{LoRAID: &id, LoRAName: "ad"}, true},
		{contract.EngineVLLM, contract.BlockStored{LoRAID: &id, LoRAName: "ad"}, true},
		{contract.EngineVLLM, contract.BlockStored{LoRAName: "ad"}, true},
		{contract.EngineVLLM, contract.BlockStored{LoRAID: &id, LoRAName: "ad", GroupIdx: &g}, true},
		{contract.EngineSGLang, contract.BlockStored{LoRAID: &id, LoRAName: "ad"}, false},
		{contract.EngineSGLang, contract.BlockStored{LoRAName: "ad"}, false},
		{contract.EngineSGLang, contract.BlockStored{LoRAID: &id}, false},
	} {
		tc.ev.Hashes, tc.ev.BlockSize = []uint64{1}, 16
		got, err := eventBatch(contract.KVBatch{Engine: tc.engine, Events: []contract.KVEvent{tc.ev}})
		if err != nil {
			t.Errorf("engine %q %+v: want it accepted, got %v", tc.engine, tc.ev, err)
			continue
		}
		s := got.Events[0].(*kvevents.BlockStoredEvent)
		if (s.LoraName != nil) != tc.wantName || (s.LoraName != nil && *s.LoraName != "ad") {
			t.Errorf("engine %q name %q: LoraName = %v, want carried = %v", tc.engine, tc.ev.LoRAName, s.LoraName, tc.wantName)
		}
		if !reflect.DeepEqual(s.LoraID, tc.ev.LoRAID) {
			t.Errorf("engine %q: LoraID = %v, want %v", tc.engine, s.LoraID, tc.ev.LoRAID)
		}
	}
}

// TestEventBatchRefusesAnEngineTheContractDoesNotDefine: a batch is decoded as its engine's
// adapter decodes it, so a kind with no adapter is refused, not read as vLLM's. The refusal
// holds for a batch with no events and for events that are otherwise valid; "" is vLLM's.
func TestEventBatchRefusesAnEngineTheContractDoesNotDefine(t *testing.T) {
	ev := []contract.KVEvent{contract.BlockStored{Hashes: []uint64{1}, BlockSize: 16}}
	for _, kind := range []string{"tensorrt", "VLLM", "SGLang", " vllm"} {
		for name, events := range map[string][]contract.KVEvent{"valid events": ev, "no events": nil} {
			_, err := eventBatch(contract.KVBatch{Engine: kind, Events: events})
			if err == nil || !strings.Contains(err.Error(), kind) || !strings.Contains(err.Error(), "does not define") {
				t.Errorf("engine %q, %s: want a refusal naming the kind, got %v", kind, name, err)
			}
		}
	}
	for _, kind := range []string{"", contract.EngineVLLM, contract.EngineSGLang} {
		if _, err := eventBatch(contract.KVBatch{Engine: kind, Events: ev}); err != nil {
			t.Errorf("engine %q: want it accepted, got %v", kind, err)
		}
	}
}
