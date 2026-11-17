/*
Copyright 2025 The Kubernetes Authors.
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

	"github.com/go-logr/logr"

	"github.com/llm-d/llm-d-router/pkg/common/hosting"
	"github.com/llm-d/llm-d-router/pkg/common/observability/logging"
	fwkdl "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/datalayer"
	"github.com/llm-d/llm-d-router/pkg/epp/metrics"
)

// collectOnce runs every dispatcher once for ep.
func collectOnce(ctx context.Context, logger logr.Logger, ep fwkdl.Endpoint, dispatchers []fwkdl.PollingDispatcher) {
	rt := hosting.From(ctx)
	for _, disp := range dispatchers {
		if ctx.Err() != nil {
			return
		}
		dispCtx, cancel := rt.WithTimeout(ctx, defaultCollectionTimeout)
		if err := disp.Dispatch(dispCtx, ep); err != nil {
			tn := disp.TypedName()
			metrics.RecordDataLayerPollError(tn.Type)
			logger.V(logging.DEBUG).Info("dispatch failed", "source", tn, "err", err)
		}
		cancel()
	}
}
