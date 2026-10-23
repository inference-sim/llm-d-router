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

	contract "github.com/inference-sim/blis-schemas/embed"

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

// eventBatch is b in vLLM's event-batch form.
func eventBatch(b contract.KVBatch) (*kvevents.EventBatch, error) {
	batch := &kvevents.EventBatch{Timestamp: float64(b.Timestamp.UnixNano()) / 1e9}
	for i, e := range b.Events {
		switch e := e.(type) {
		case contract.BlockStored:
			batch.Events = append(batch.Events, blockStored(e))
		case *contract.BlockStored:
			batch.Events = append(batch.Events, blockStored(*e))
		case contract.BlockRemoved:
			batch.Events = append(batch.Events, blockRemoved(e))
		case *contract.BlockRemoved:
			batch.Events = append(batch.Events, blockRemoved(*e))
		default:
			return nil, fmt.Errorf("KV event %d: unknown event type %T", i, e)
		}
	}
	return batch, nil
}

func blockStored(e contract.BlockStored) *kvevents.BlockStoredEvent {
	out := &kvevents.BlockStoredEvent{BlockHashes: e.Hashes, ParentHash: e.Parent, BlockSize: e.BlockSize, DeviceTier: e.Tier}
	if len(e.Tokens) > 0 {
		out.Tokens = e.Tokens
	}
	return out
}

func blockRemoved(e contract.BlockRemoved) *kvevents.BlockRemovedEvent {
	return &kvevents.BlockRemovedEvent{BlockHashes: e.Hashes, DeviceTier: e.Tier}
}

// hostRT is the hosting runtime on the router's context.
func hostRT(ctx context.Context) hosting.Runtime { return hosting.From(ctx) }
