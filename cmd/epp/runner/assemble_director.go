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
	"github.com/llm-d/llm-d-router/pkg/epp/datastore"
	"github.com/llm-d/llm-d-router/pkg/epp/flowcontrol/contracts"
	fceviction "github.com/llm-d/llm-d-router/pkg/epp/flowcontrol/eviction"
	fwkplugin "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/plugin"
	"github.com/llm-d/llm-d-router/pkg/epp/requestcontrol"
	"github.com/llm-d/llm-d-router/pkg/epp/scheduling"
	runserver "github.com/llm-d/llm-d-router/pkg/epp/server"
)

// assembleDirector builds the request path of a router without a Kubernetes cluster: it validates
// plugin stability, configures the data layer, and wires the scheduler, admission control and
// Director over ds.
func (r *Runner) assembleDirector(ctx context.Context, opts *runserver.Options, eppConfig *config.Config, ds datastore.Datastore) (
	*requestcontrol.Director, contracts.PriorityBandControlPlane, *fceviction.RequestEvictor, error) {
	if err := fwkplugin.ValidatePluginStability(r.PluginHandle, opts.AllowExperimentalPlugins); err != nil {
		setupLog.Error(err, "Plugin stability validation failed")
		return nil, nil, nil, err
	}

	if err := r.dlRuntime.Configure(eppConfig.DataConfig, setupLog); err != nil {
		return nil, nil, nil, fmt.Errorf("failed to configure datalayer: %w", err)
	}

	if r.schedulerConfig == nil {
		return nil, nil, nil, errors.New("scheduler config must be set either by config api or through code")
	}
	setupLog.Info("parsed config", "scheduler-config", r.schedulerConfig)

	scheduler := scheduling.NewSchedulerWithConfig(r.schedulerConfig)

	// Outside Kubernetes there is no InferenceObjective CRD, so per-request
	// priority falls back to Director.defaultPriority (see
	// pkg/epp/requestcontrol/director.go); static bands defined in
	// EndpointPickerConfig.flowControl still apply.
	endpointCandidates := contracts.EndpointCandidates(requestcontrol.NewDatastoreEndpointCandidates(ds,
		requestcontrol.WithDisableEndpointSubsetFilter(opts.DisableEndpointSubsetFilter)))
	endpointCandidates, admissionController, controlPlane, requestEvictor := r.initAdmissionControl(ctx, opts, eppConfig, endpointCandidates)
	director := requestcontrol.NewDirectorWithConfig(ds, scheduler, admissionController, endpointCandidates, r.requestControlConfig)
	if requestEvictor != nil {
		director.SetRequestEvictor(requestEvictor)
	}
	return director, controlPlane, requestEvictor, nil
}
