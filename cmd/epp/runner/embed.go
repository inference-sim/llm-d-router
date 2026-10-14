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

package runner

import (
	"context"
	"errors"
	"fmt"

	"github.com/llm-d/llm-d-router/pkg/epp/config"
	"github.com/llm-d/llm-d-router/pkg/epp/datalayer"
	"github.com/llm-d/llm-d-router/pkg/epp/datastore"
	"github.com/llm-d/llm-d-router/pkg/epp/flowcontrol"
	"github.com/llm-d/llm-d-router/pkg/epp/flowcontrol/contracts"
	fceviction "github.com/llm-d/llm-d-router/pkg/epp/flowcontrol/eviction"
	fwkplugin "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/plugin"
	"github.com/llm-d/llm-d-router/pkg/epp/handlers"
	"github.com/llm-d/llm-d-router/pkg/epp/requestcontrol"
	runserver "github.com/llm-d/llm-d-router/pkg/epp/server"
)

// EmbedOptions configures Embed.
type EmbedOptions struct {
	// Options are the EPP command-line options, already completed and validated.
	Options *runserver.Options
	// Pool is the InferencePool the router serves. Endpoints are added to Embedded.Datastore by
	// the embedder.
	Pool *datalayer.EndpointPool
	// AfterRegister runs after the in-tree plugins are registered and before the configuration is
	// instantiated. An embedder uses it to register replacement factories for plugin types whose
	// real implementation depends on external services.
	AfterRegister func() error
}

// Embedded is a router assembled in-process from the EPP startup path, without the ext_proc,
// health and metrics servers, without Kubernetes reconcilers, and without endpoint discovery.
type Embedded struct {
	Director       *requestcontrol.Director
	Datastore      datastore.Datastore
	Handle         fwkplugin.Handle
	DataLayer      *datalayer.Runtime
	Parsers        *handlers.ParserRegistry
	Config         *config.Config
	FeatureGates   map[string]bool
	RequestEvictor *fceviction.RequestEvictor
	// ControlPlane is non-nil when flow control is enabled.
	ControlPlane contracts.PriorityBandControlPlane
}

// FlowControlEnabled reports whether the flowControl feature gate is on.
func (e *Embedded) FlowControlEnabled() bool { return e.FeatureGates[flowcontrol.FeatureGate] }

// Embed builds a router the way Run does for file discovery, with ctx as the lifetime of every
// background loop. ctx carries the hosting runtime the router runs on.
func Embed(ctx context.Context, eo EmbedOptions) (*Embedded, error) {
	if eo.Options == nil || eo.Pool == nil {
		return nil, errors.New("embed: options and pool are required")
	}
	r := NewRunner()
	opts := eo.Options
	fwkplugin.SetDefaultStalenessThreshold(opts.PluginStateStalenessThreshold)

	rawConfig, err := r.parseConfigurationPhaseOne(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to parse configuration: %w", err)
	}
	if eo.AfterRegister != nil {
		if err := eo.AfterRegister(); err != nil {
			return nil, err
		}
	}

	epf := r.setupMetricsCollection(opts)
	ds := datastore.NewDatastore(ctx, epf).WithEndpointPool(eo.Pool)

	eppConfig, err := r.parseConfigurationPhaseTwo(ctx, rawConfig, ds, opts.RefreshMetricsInterval)
	if err != nil {
		return nil, fmt.Errorf("failed to parse configuration: %w", err)
	}
	director, controlPlane, requestEvictor, err := r.assembleDirector(ctx, opts, eppConfig, ds)
	if err != nil {
		return nil, err
	}

	return &Embedded{
		Director:       director,
		Datastore:      ds,
		Handle:         r.PluginHandle,
		DataLayer:      r.dlRuntime,
		Parsers:        r.parserRegistry,
		Config:         eppConfig,
		FeatureGates:   r.featureGates,
		RequestEvictor: requestEvictor,
		ControlPlane:   controlPlane,
	}, nil
}
