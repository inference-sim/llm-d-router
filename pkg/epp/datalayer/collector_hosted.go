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

package datalayer

import (
	"context"
	"errors"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/llm-d/llm-d-router/pkg/common/hosting"
	fwkdl "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/datalayer"
)

// startHosted is Start for a hosted runtime: collection runs on the runtime's schedule instead of
// on a goroutine of its own.
func (c *Collector) startHosted(ctx context.Context, rt hosting.Runtime, interval time.Duration, ep fwkdl.Endpoint, dispatchers []fwkdl.PollingDispatcher) error {
	if len(dispatchers) == 0 {
		return errors.New("cannot start collector with empty dispatchers")
	}
	for _, d := range dispatchers {
		if d == nil {
			return errors.New("cannot add nil dispatcher")
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancel != nil {
		return errors.New("collector start called multiple times")
	}
	ctx, cancel := context.WithCancel(ctx)
	c.cancel = cancel
	close(c.done) // no goroutine to wait for in Stop
	logger := log.FromContext(ctx).WithValues("endpoint", ep.GetMetadata().GetIPAddress())
	rt.Every(ctx, "datalayer-collector/"+ep.GetMetadata().GetID().String(), interval, func(ctx context.Context) {
		collectOnce(ctx, logger, ep, dispatchers)
	})
	return nil
}
