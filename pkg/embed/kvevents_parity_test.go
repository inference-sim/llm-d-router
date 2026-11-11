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
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/vmihailenco/msgpack/v5"

	contract "github.com/inference-sim/blis-substrate/contracts/embed"

	"github.com/llm-d/llm-d-router/pkg/kvevents"
	"github.com/llm-d/llm-d-router/pkg/kvevents/engineadapter"
)

// adapterEvents is what llm-d's own adapter makes of one engine message carrying events.
func adapterEvents(t *testing.T, a kvevents.EngineAdapter, events ...any) []kvevents.GenericEvent {
	t.Helper()
	payload, err := msgpack.Marshal([]any{1.5, events, nil})
	if err != nil {
		t.Fatal(err)
	}
	_, _, batch, err := a.ParseMessage(&kvevents.RawMessage{Topic: "kv@pod@m", Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	return batch.Events
}

func embedEvents(t *testing.T, events ...contract.KVEvent) []kvevents.GenericEvent {
	t.Helper()
	return embedEventsFrom(t, "", events...)
}

// embedEventsFrom is embedEvents for a batch that names its engine.
func embedEventsFrom(t *testing.T, engine string, events ...contract.KVEvent) []kvevents.GenericEvent {
	t.Helper()
	b, err := eventBatch(contract.KVBatch{Engine: engine, Events: events})
	if err != nil {
		t.Fatal(err)
	}
	return b.Events
}

func requireSame(t *testing.T, got, want []kvevents.GenericEvent) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("eventBatch =\n  %s\nllm-d's adapter =\n  %s", dump(got), dump(want))
	}
}

func dump(events []kvevents.GenericEvent) string {
	var parts []string
	for _, e := range events {
		parts = append(parts, fmt.Sprintf("%#v", e))
		if s, ok := e.(*kvevents.BlockStoredEvent); ok && s.LoraName != nil {
			parts = append(parts, "lora_name="+*s.LoraName)
		}
	}
	return strings.Join(parts, "; ")
}

// TestEventBatchMatchesLLMDAdapters: the events eventBatch makes from the contract's events are,
// field for field (reflect.DeepEqual), the events llm-d's own adapters decode from the messages
// the engines publish. This is the evidence that carrying the widened fields is the fork's
// hosted-KV-fidelity change, not a new behaviour: the router indexes what llm-d indexes.
func TestEventBatchMatchesLLMDAdapters(t *testing.T) {
	vllm := engineadapter.NewVLLMAdapter()
	sglang := engineadapter.NewSGLangAdapter()
	id, group, window, zero := 4, 2, 128, 0

	t.Run("vLLM full BlockStored: lora id and name, extra keys, group, sliding window", func(t *testing.T) {
		keys := [][]any{{"ad", "mm"}, nil}
		got := embedEventsFrom(t, contract.EngineVLLM, contract.BlockStored{Hashes: []uint64{1, 2}, Parent: 9, Tokens: []uint32{1, 2, 3, 4},
			BlockSize: 2, Tier: "cpu", LoRAID: &id, LoRAName: "ad", ExtraKeys: keys, GroupIdx: &group,
			SpecKind: contract.SpecSlidingWindow, SlidingWindow: &window})
		want := adapterEvents(t, vllm, []any{"BlockStored", []any{uint64(1), uint64(2)}, uint64(9),
			[]uint32{1, 2, 3, 4}, 2, id, "cpu", "ad", keys, group, "sliding_window", window})
		requireSame(t, got, want)
	})
	t.Run("vLLM BlockStored without the optional fields", func(t *testing.T) {
		got := embedEvents(t, contract.BlockStored{Hashes: []uint64{5}, Tokens: []uint32{7, 8}, BlockSize: 2})
		want := adapterEvents(t, vllm, []any{"BlockStored", []any{uint64(5)}, nil, []uint32{7, 8}, 2, nil, nil, nil, nil, nil, nil, nil})
		requireSame(t, got, want)
	})
	t.Run("vLLM group zero is a group, not an absent one", func(t *testing.T) {
		got := embedEvents(t, contract.BlockStored{Hashes: []uint64{5}, Tokens: []uint32{7, 8}, BlockSize: 2, GroupIdx: &zero},
			contract.BlockRemoved{Hashes: []uint64{5}, GroupIdx: &zero})
		want := adapterEvents(t, vllm,
			[]any{"BlockStored", []any{uint64(5)}, nil, []uint32{7, 8}, 2, nil, nil, nil, nil, zero},
			[]any{"BlockRemoved", []any{uint64(5)}, nil, zero})
		requireSame(t, got, want)
	})
	t.Run("vLLM BlockRemoved with a tier and a group", func(t *testing.T) {
		got := embedEvents(t, contract.BlockRemoved{Hashes: []uint64{1, 2}, Tier: "cpu", GroupIdx: &group})
		want := adapterEvents(t, vllm, []any{"BlockRemoved", []any{uint64(1), uint64(2)}, "cpu", group})
		requireSame(t, got, want)
	})
	t.Run("vLLM AllBlocksCleared", func(t *testing.T) {
		got := embedEvents(t, contract.AllBlocksCleared{})
		want := adapterEvents(t, vllm, []any{"AllBlocksCleared"})
		requireSame(t, got, want)
	})
	t.Run("SGLang BlockStored with lora_id and lora_name: the name is dropped, as llm-d reads only the id", func(t *testing.T) {
		seven := 7
		// SGLang's lora_name is in the map form's payload; llm-d's adapter never reads it.
		want := adapterEvents(t, sglang, map[string]any{"type": "BlockStored", "block_hashes": []any{uint64(1)},
			"token_ids": []uint32{1, 2}, "block_size": 2, "lora_id": seven, "lora_name": "ad"})
		got := embedEventsFrom(t, contract.EngineSGLang,
			contract.BlockStored{Hashes: []uint64{1}, Tokens: []uint32{1, 2}, BlockSize: 2, LoRAID: &seven, LoRAName: "ad"})
		requireSame(t, got, want)
	})
	t.Run("SGLang cache salt on the first block of a run with no parent", func(t *testing.T) {
		got := embedEventsFrom(t, contract.EngineSGLang,
			contract.BlockStored{Hashes: []uint64{2, 3}, Tokens: []uint32{1, 2, 3, 4}, BlockSize: 2, CacheSalt: "s"},
			contract.BlockStored{Hashes: []uint64{4}, Parent: 3, Tokens: []uint32{5, 6}, BlockSize: 2, CacheSalt: "s"})
		want := adapterEvents(t, sglang,
			map[string]any{"type": "BlockStored", "block_hashes": []any{uint64(2), uint64(3)}, "token_ids": []uint32{1, 2, 3, 4},
				"block_size": 2, "cache_salt": "s"},
			map[string]any{"type": "BlockStored", "block_hashes": []any{uint64(4)}, "parent_block_hash": uint64(3),
				"token_ids": []uint32{5, 6}, "block_size": 2, "cache_salt": "s"})
		requireSame(t, got, want)
	})
	t.Run("SGLang BlockRemoved and AllBlocksCleared", func(t *testing.T) {
		got := embedEventsFrom(t, contract.EngineSGLang, contract.BlockRemoved{Hashes: []uint64{1}, Tier: "cpu"}, contract.AllBlocksCleared{})
		want := adapterEvents(t, sglang, []any{"BlockRemoved", []any{uint64(1)}, "cpu"}, []any{"AllBlocksCleared"})
		requireSame(t, got, want)
	})
}

// TestEventBatchDiffersFromLLMDOnPurpose names where the conversion deliberately goes beyond
// what llm-d's adapters decode.
func TestEventBatchDiffersFromLLMDOnPurpose(t *testing.T) {
	// Neither engine emits a tier on AllBlocksCleared (llm-d's adapters drop none because they
	// read none); the contract carries one for a host that clears a single tier. The pool clears
	// every tier of the pod whatever the tier, so the index agrees with llm-d's.
	got := embedEvents(t, contract.AllBlocksCleared{Tier: "cpu"})
	want := []kvevents.GenericEvent{&kvevents.AllBlocksClearedEvent{DeviceTier: "cpu"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("a cleared tier: got %#v want %#v", got, want)
	}
}
