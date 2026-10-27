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
	"cmp"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	contract "github.com/inference-sim/blis-schemas/embed"

	"github.com/llm-d/llm-d-router/pkg/epp/config"
	fwkplugin "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/plugin"
)

// refusedPrefix starts every refusal of a configuration the hosted router cannot run as the
// deployment would.
const refusedPrefix = "the router configuration cannot be simulated faithfully: "

// notAudited is the reason a plugin type outside the audited set is refused.
const notAudited = "it has not been audited for the hosting runtime"

// audited lists the plugin types the fork hosts: every clock read, background loop, blocking
// wait and random draw such a plugin makes on its decision path goes through hosting.Runtime
// (the hosting-lint baseline holds no unrouted use of theirs). Any other type is refused before
// its constructor runs, whatever the host allows.
var audited = map[string]bool{
	// profile handlers and pickers
	"single-profile-handler": true,
	"disagg-profile-handler": true,
	"max-score-picker":       true,
	"random-picker":          true,
	"weighted-random-picker": true,
	// filters
	"decode-filter":                    true,
	"prefill-filter":                   true,
	"encode-filter":                    true,
	"always-disagg-multimodal-decider": true,
	"label-selector-filter":            true,
	"topology-affinity-filter":         true,
	"topology-stamp-handler":           true,
	// scorers
	"queue-scorer":                 true,
	"kv-cache-utilization-scorer":  true,
	"prefix-cache-scorer":          true,
	"load-aware-scorer":            true,
	"running-requests-size-scorer": true,
	"lora-affinity-scorer":         true,
	"token-load-scorer":            true,
	"no-hit-lru-scorer":            true,
	"active-request-scorer":        true,
	"context-length-aware":         true,
	"topology-affinity-scorer":     true,
	// P/D deciders
	"always-disagg-pd-decider": true,
	"prefix-based-pd-decider":  true,
	// data producers
	"approx-prefix-cache-producer": true,
	"inflight-load-producer":       true,
	"token-producer":               true,
	// KV events reach it through IngestKVEvents (Producer.HostedIngest); hostingRefusals refuses
	// the index backends a hosted runtime cannot run.
	"precise-prefix-cache-producer": true,
	// admission
	"probabilistic-admitter": true,
	// saturation detectors
	"utilization-detector":    true,
	"concurrency-detector":    true,
	"max-saturation-detector": true,
	// flow control
	"global-strict-fairness-policy":            true,
	"round-robin-fairness-policy":              true,
	"fcfs-ordering-policy":                     true,
	"edf-ordering-policy":                      true,
	"slo-deadline-ordering-policy":             true,
	"static-usage-limit-policy":                true,
	"static-usage-limit-policy-type":           true, // the policy's TypedName type
	"strict-band-selection-policy":             true,
	"priority-holdback-policy":                 true,
	"soft-reflective-ceiling-policy":           true,
	"sheddable-eviction-filter":                true,
	"priority-then-time-eviction-order-policy": true,
	// data layer
	"metrics-data-source":          true,
	"core-metrics-extractor":       true,
	"endpoint-notification-source": true,
	"topology-extractor":           true,
	// request parsing: pure functions of the request and response bytes
	"openai-parser":      true,
	"vllmhttp-parser":    true,
	"sglanghttp-parser":  true,
	"anthropic-parser":   true,
	"passthrough-parser": true,
	"vertexai-parser":    true,
}

// hostingReasons are plugin types the fork cannot host, with why: they read the wall clock,
// draw from process-wide randomness, run unrouted background work, or reach a service a hosted
// router has no access to. They are refused with this reason before the host's policy is asked.
var hostingReasons = map[string]string{
	"burst-prefix-cache-producer":       "it blocks a request on a sealing timer the hosting runtime does not model",
	"predicted-latency-producer":        "it calls an external latency-predictor service",
	"latency-scorer":                    "it consumes the external latency predictor",
	"latency-slo-admitter":              "it consumes the external latency predictor",
	"slo-headroom-tier-filter":          "it consumes the external latency predictor",
	"prefix-cache-affinity-filter":      "it consumes the external latency predictor",
	"latency-observer-producer-hub":     "its clock reads have not been moved onto the hosting runtime",
	"latency-observation-scorer-hub":    "its exploration draws from process-wide randomness",
	"session-state-producer":            "its clock reads have not been moved onto the hosting runtime",
	"program-aware-fairness":            "its background eviction loop and clock reads have not been moved onto the hosting runtime",
	"thunder-agent":                     "it calls an external agent service",
	"redis-syncer":                      "it synchronizes through an external Redis",
	"k8s-notification-source":           "it needs a Kubernetes controller manager",
	"k8s-peer-discovery":                "it needs a Kubernetes API server",
	"disaggregatedset-rollout-screener": "its revision split draws from process-wide randomness",
	"utilization-filter":                "it has not been audited for the hosting runtime",
	"request-attribute-reporter":        "it has not been audited for the hosting runtime",
}

// policy decides which plugin types a router instance may instantiate.
type policy struct {
	allow func(pluginType string) (ok bool, reason string)
}

// refusal returns why a plugin of type typ is refused, or ok when it may be instantiated. In
// order: a type the fork cannot host is refused with its own reason; otherwise the host's
// policy is asked, and a type it refuses is refused with its reason; otherwise a type outside
// the audited set is refused as not audited.
func (p policy) refusal(typ string) (reason string, ok bool) {
	if reason, refused := hostingReasons[typ]; refused {
		return reason, false
	}
	if p.allow != nil {
		if ok, reason := p.allow(typ); !ok {
			return cmp.Or(reason, "the host's plugin policy refuses it"), false
		}
	}
	if !audited[typ] {
		return notAudited, false
	}
	return "", true
}

// refuseFactories wraps the factory of every registered plugin type so that a type the policy
// refuses fails with a *RefusalError before its real constructor runs: before it starts a
// goroutine, a timer or any external work. The router instantiates every plugin through its
// factory, whether the configuration names it under plugins:, reaches it only through a
// reference, or adds it as a default. It runs as runner.EmbedOptions.AfterRegister, under embedMu,
// and returns the function that restores the real factories, which the caller runs once the
// configuration is instantiated, so no policy outlives its router in the process-wide registry.
func (p policy) refuseFactories() (restore func()) {
	saved := make(map[string]fwkplugin.FactoryFunc, len(fwkplugin.Registry))
	for typ, factory := range fwkplugin.Registry { // one independent write per key
		saved[typ] = factory
		fwkplugin.Registry[typ] = func(name string, params *json.Decoder, handle fwkplugin.Handle) (fwkplugin.Plugin, error) {
			if reason, ok := p.refusal(typ); !ok {
				return nil, fmt.Errorf(refusedPrefix+"%w", &contract.RefusalError{Plugin: name, Type: typ, Reason: reason})
			}
			return factory(name, params, handle)
		}
	}
	return func() {
		for typ, factory := range saved { // one independent write per key
			fwkplugin.Registry[typ] = factory
		}
	}
}

// refusals is every plugin a constructed router holds that the policy refuses, sorted by text.
type refusals []*contract.RefusalError

func (r refusals) Error() string {
	lines := make([]string, len(r))
	for i, e := range r {
		lines[i] = e.Error()
	}
	return strings.TrimSuffix(refusedPrefix, " ") + "\n  " + strings.Join(lines, "\n  ")
}

func (r refusals) Unwrap() []error {
	out := make([]error, len(r))
	for i, e := range r {
		out[i] = e
	}
	return out
}

// validateEmbedded is a backstop behind refuseFactories: every plugin the constructed router
// holds, by any path (the handle's named plugins and every plugin its configuration references),
// is one the policy allows.
func (p policy) validateEmbedded(cfg *config.Config, handle fwkplugin.Handle) error {
	var out refusals
	for name, typ := range embeddedPlugins(cfg, handle) { // sorted below
		if reason, ok := p.refusal(typ); !ok {
			out = append(out, &contract.RefusalError{Plugin: name, Type: typ, Reason: reason})
		}
	}
	if len(out) == 0 {
		return nil
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Error() < out[j].Error() })
	return out
}

// embeddedPlugins returns every plugin the router holds, by name, with its type: the handle's
// named plugins and every plugin the loaded configuration references (a flow-control policy or
// saturation detector named by a ref, a data source, ...), found by walking the configuration.
func embeddedPlugins(cfg *config.Config, handle fwkplugin.Handle) map[string]string {
	out := map[string]string{}
	for name, p := range handle.GetAllPluginsWithNames() { // one independent write per key
		out[name] = p.TypedName().Type
	}
	pluginType := reflect.TypeFor[fwkplugin.Plugin]()
	seen := map[uintptr]bool{}
	var walk func(v reflect.Value)
	walk = func(v reflect.Value) {
		if !v.IsValid() {
			return
		}
		if v.Kind() == reflect.Pointer || v.Kind() == reflect.Map || v.Kind() == reflect.Slice {
			if v.IsNil() {
				return
			}
			if v.Kind() != reflect.Slice {
				if seen[v.Pointer()] {
					return
				}
				seen[v.Pointer()] = true
			}
		}
		if v.Kind() == reflect.Interface {
			if v.IsNil() {
				return
			}
			v = v.Elem()
		}
		if v.CanInterface() && v.Type().Implements(pluginType) {
			if p, ok := v.Interface().(fwkplugin.Plugin); ok && (v.Kind() != reflect.Pointer || !v.IsNil()) {
				tn := p.TypedName()
				out[cmp.Or(tn.Name, tn.Type)] = tn.Type
				return // a plugin's internals are its own
			}
		}
		switch v.Kind() {
		case reflect.Pointer:
			walk(v.Elem())
		case reflect.Struct:
			for i := range v.NumField() {
				if v.Type().Field(i).IsExported() {
					walk(v.Field(i))
				}
			}
		case reflect.Slice, reflect.Array:
			for i := range v.Len() {
				walk(v.Index(i))
			}
		case reflect.Map:
			for it := v.MapRange(); it.Next(); {
				walk(it.Value())
			}
		}
	}
	walk(reflect.ValueOf(cfg))
	return out
}
