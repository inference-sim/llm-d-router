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

package kvevents

import (
	"context"
	"errors"

	"github.com/llm-d/llm-d-router/pkg/kvcache/kvblock"
	"github.com/llm-d/llm-d-router/pkg/kvcache/metrics"
)

// NewHostedPool creates a Pool for a router embedded by a host that delivers KV events itself
// (see pkg/common/hosting). The pool has no queues and no workers: events reach it only through
// ProcessEventBatch and ClearPod, which apply them synchronously on the caller's goroutine. It
// must not be handed to a SubscriberManager, and Start and AddTask must not be called on it.
//
// Like NewPool, it registers the kvcache metrics. cfg selects event tracing only; Concurrency is
// ignored because no worker runs.
func NewHostedPool(cfg *Config, index kvblock.Index, tokenProcessor kvblock.TokenProcessor,
	adapter EngineAdapter,
) (*Pool, error) {
	if cfg == nil {
		cfg = DefaultConfig()
	}
	if index == nil {
		return nil, errors.New("kvevents: hosted pool needs an index")
	}
	if tokenProcessor == nil {
		return nil, errors.New("kvevents: hosted pool needs a token processor")
	}

	p := &Pool{
		index:          index,
		tokenProcessor: tokenProcessor,
		adapter:        adapter,
		groupCatalog:   kvblock.NewGroupCatalog(),
		dedup:          newEventDedupFilter(),
		tracer:         newEventTracer(cfg.Tracing),
	}

	metrics.Register()

	return p, nil
}

// ProcessEventBatch applies batch to the index synchronously, as a worker applies a decoded
// message: podIdentifier is the serving endpoint ("address:port") the events describe and
// modelName the model the engine published them under (a BlockStored event's LoraName, when set,
// takes its place). The caller must deliver one pod's batches in the order the engine emitted them.
// Events the pool cannot apply are skipped and logged, as on the worker path; the error reports
// only a missing batch or a done ctx.
func (p *Pool) ProcessEventBatch(ctx context.Context, batch *EventBatch, podIdentifier, modelName string) error {
	if batch == nil {
		return errors.New("kvevents: nil event batch")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ctx, span := p.startSpan(ctx, "events_process", internalSpanOptions)
	defer span.End()
	p.processEventBatch(ctx, batch, podIdentifier, modelName)
	return nil
}

// ClearPod drops every index entry of podIdentifier across all device tiers, as an
// AllBlocksCleared event from that pod does.
func (p *Pool) ClearPod(ctx context.Context, podIdentifier string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.clearPod(ctx, podIdentifier, false)
	return nil
}
