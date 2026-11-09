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
	"errors"
	"fmt"
	"sort"

	contract "github.com/inference-sim/blis-substrate/contracts/embed"

	"github.com/llm-d/llm-d-router/pkg/common/hosting"
	fwkplugin "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/plugin"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/requestcontrol/dataproducer/preciseprefixcache"
	"github.com/llm-d/llm-d-router/pkg/kvevents"
)

// KV events: an engine's prefix-cache changes reach the configuration's
// precise-prefix-cache-producers synchronously (Producer.HostedIngest), exactly as the producer
// would index vLLM's events from its ZMQ subscriber.

// kvIngesters are the configuration's precise-prefix-cache-producers, in name order.
func kvIngesters(handle fwkplugin.Handle) []*preciseprefixcache.Producer {
	all := handle.GetAllPluginsWithNames()
	names := make([]string, 0, len(all))
	for name := range all { // sorted below
		names = append(names, name)
	}
	sort.Strings(names)
	var out []*preciseprefixcache.Producer
	for _, name := range names {
		if p, ok := all[name].(*preciseprefixcache.Producer); ok {
			out = append(out, p)
		}
	}
	return out
}

// WantsKVEvents implements embed.Router.
func (r *Router) WantsKVEvents() bool { return len(r.kvIngest) > 0 }

// IngestKVEvents implements embed.Router: the batch is delivered to every
// precise-prefix-cache-producer, in name order. The first error stops the delivery.
func (r *Router) IngestKVEvents(_ context.Context, endpoint, model string, b contract.KVBatch) error {
	batch, err := eventBatch(b)
	if err != nil {
		return err
	}
	for _, p := range r.kvIngest {
		if err := p.HostedIngest(r.ctx, endpoint, model, batch); err != nil {
			return err
		}
	}
	return nil
}

// ClearKVEvents implements embed.Router.
func (r *Router) ClearKVEvents(_ context.Context, endpoint string) error {
	for _, p := range r.kvIngest {
		if err := p.HostedClearPod(r.ctx, endpoint); err != nil {
			return err
		}
	}
	return nil
}

// KVBlocks implements embed.KVIndexInspector on the first precise-prefix-cache-producer in name
// order.
func (r *Router) KVBlocks(endpoint, tier string) (map[uint64]bool, error) {
	if len(r.kvIngest) == 0 {
		return nil, errors.New("the router configuration has no precise-prefix-cache-producer")
	}
	if tier == "" {
		return r.kvIngest[0].HostedPodBlocks(endpoint)
	}
	return r.kvIngest[0].HostedPodTierBlocks(endpoint, tier)
}

// eventBatch is b in vLLM's event-batch form, each event as llm-d's own engine adapters decode
// the engine's message (engineadapter.VLLMAdapter, engineadapter.SGLangAdapter). The parity
// test pins that.
func eventBatch(b contract.KVBatch) (*kvevents.EventBatch, error) {
	batch := &kvevents.EventBatch{Timestamp: float64(b.Timestamp.UnixNano()) / 1e9}
	for i, e := range b.Events {
		var (
			ev  kvevents.GenericEvent
			err error
		)
		switch e := e.(type) {
		case contract.BlockStored:
			ev, err = blockStored(e)
		case *contract.BlockStored:
			ev, err = blockStored(*e)
		case contract.BlockRemoved:
			ev, err = blockRemoved(e)
		case *contract.BlockRemoved:
			ev, err = blockRemoved(*e)
		case contract.AllBlocksCleared:
			ev = allBlocksCleared(e)
		case *contract.AllBlocksCleared:
			ev = allBlocksCleared(*e)
		default:
			return nil, fmt.Errorf("KV event %d: unknown event type %T", i, e)
		}
		if err != nil {
			return nil, fmt.Errorf("KV event %d: %w", i, err)
		}
		batch.Events = append(batch.Events, ev)
	}
	return batch, nil
}

// checkGroup refuses a negative KV-cache group, as vLLM's decoder does: -1 is the dedup
// filter's no-group sentinel, and a group is never negative.
func checkGroup(g *int) error {
	if g != nil && *g < 0 {
		return fmt.Errorf("group_idx: negative value: %d", *g)
	}
	return nil
}

// vllmOnly reports whether the event carries a field only vLLM emits (the contract's field
// comments name the engine of each).
func vllmOnly(e contract.BlockStored) bool {
	return e.ExtraKeys != nil || e.GroupIdx != nil || e.SpecKind != "" || e.SlidingWindow != nil
}

func blockStored(e contract.BlockStored) (*kvevents.BlockStoredEvent, error) {
	if err := checkGroup(e.GroupIdx); err != nil {
		return nil, err
	}
	if e.CacheSalt != "" && e.ExtraKeys != nil {
		return nil, errors.New("both a cache salt and extra keys: an engine sets one of the two " +
			"(SGLang the salt, vLLM the keys)")
	}
	out := &kvevents.BlockStoredEvent{
		BlockHashes: e.Hashes, ParentHash: e.Parent, BlockSize: e.BlockSize, DeviceTier: e.Tier,
		LoraID: e.LoRAID, ExtraKeys: e.ExtraKeys, GroupIdx: e.GroupIdx,
		KVCacheSpecKind:              kvevents.KVCacheSpecKind(e.SpecKind),
		KVCacheSpecSlidingWindowSize: e.SlidingWindow,
	}
	if len(e.Tokens) > 0 {
		out.Tokens = e.Tokens
	}
	// llm-d's vLLM adapter reads lora_name; its SGLang adapter does not (it reads lora_id only),
	// and the pool takes the name as the model of the blocks. KVBatch does not say which engine
	// published it, so the engine is read from the fields: a name with fields only vLLM emits,
	// or with no lora_id (SGLang always sends the id), is vLLM's and kept; a name with a lora_id
	// and nothing of vLLM's is either engine's and is refused rather than guessed. The contract
	// needs a KVBatch.Engine to end the refusal.
	if e.LoRAName != "" {
		if e.LoRAID != nil && !vllmOnly(e) {
			return nil, errors.New("lora_name with a lora_id and no field only vLLM emits: the batch does not say " +
				"whether vLLM (which indexes the blocks under the name) or SGLang (whose name llm-d ignores) " +
				"published it")
		}
		name := e.LoRAName
		out.LoraName = &name
	}
	if e.CacheSalt != "" && e.Parent == 0 && len(e.Hashes) > 0 {
		// SGLang's adapter folds the salt into the first block of a run with no parent
		// (extra_keys layout); events with a parent inherit it through the parent's key.
		out.ExtraKeys = make([][]any, len(e.Hashes))
		out.ExtraKeys[0] = []any{e.CacheSalt}
	}
	return out, nil
}

func blockRemoved(e contract.BlockRemoved) (*kvevents.BlockRemovedEvent, error) {
	if err := checkGroup(e.GroupIdx); err != nil {
		return nil, err
	}
	return &kvevents.BlockRemovedEvent{BlockHashes: e.Hashes, DeviceTier: e.Tier, GroupIdx: e.GroupIdx}, nil
}

// allBlocksCleared carries the tier through. The pool clears every tier of the pod whatever the
// tier, because the index cannot scope a clear (pool.go logs the wider clear).
func allBlocksCleared(e contract.AllBlocksCleared) *kvevents.AllBlocksClearedEvent {
	return &kvevents.AllBlocksClearedEvent{DeviceTier: e.Tier}
}

// hostRT is the hosting runtime on the router's context.
func hostRT(ctx context.Context) hosting.Runtime { return hosting.From(ctx) }
