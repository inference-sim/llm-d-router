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
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	contract "github.com/inference-sim/blis-schemas/embed"
	"github.com/inference-sim/blis-schemas/embed/embedtest"

	fwkplugin "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/plugin"
)

// defaultFlags select a standalone pool of every pod labeled for serving.
var defaultFlags = []string{"--endpoint-selector=llm-d.ai/inference-serving=true", "--endpoint-target-ports=8000"}

func build(t *testing.T, cfg contract.Config) (contract.Router, error) {
	t.Helper()
	r, err := New(context.Background(), embedtest.NewRuntime(1), cfg)
	if err == nil {
		t.Cleanup(r.Stop)
	}
	return r, err
}

// configHeader is an EndpointPickerConfig's type.
const configHeader = "apiVersion: llm-d.ai/v1\nkind: EndpointPickerConfig\n"

// withConfig is a standalone pool with the configuration epc (its type header added).
func withConfig(epc string) contract.Config {
	if epc != "" && !strings.HasPrefix(epc, "apiVersion") && epc != "[:" {
		epc = configHeader + epc
	}
	return contract.Config{Flags: defaultFlags, Config: []byte(epc)}
}

// TestUnsupportedPluginIsRefusedBeforeItsConstructorRuns: a plugin type the fork has not audited
// is refused at its factory, so its real constructor (which may start goroutines, timers or
// external work) never runs, with an error that wraps a RefusalError naming it (F6).
func TestUnsupportedPluginIsRefusedBeforeItsConstructorRuns(t *testing.T) {
	const typ = "embed-test-constructor-recorder"
	ran := false
	fwkplugin.Register(typ, fwkplugin.StabilityStable, func(string, *json.Decoder, fwkplugin.Handle) (fwkplugin.Plugin, error) {
		ran = true
		return nil, errors.New("the real constructor ran")
	})
	t.Cleanup(func() { delete(fwkplugin.Registry, typ); delete(fwkplugin.RegistryMetadata, typ) })
	cfg := withConfig("plugins:\n- type: " + typ + "\n- type: queue-scorer\n- type: max-score-picker\n- type: single-profile-handler\n" +
		"schedulingProfiles:\n- name: default\n  plugins:\n  - pluginRef: queue-scorer\n  - pluginRef: max-score-picker\n")
	_, err := build(t, cfg)
	var re *contract.RefusalError
	if !errors.As(err, &re) || re.Type != typ || re.Reason != notAudited {
		t.Fatalf("want %s refused as not audited, got %v", typ, err)
	}
	if !strings.HasPrefix(err.Error(), "llm-d router: ") || !strings.Contains(err.Error(), refusedPrefix+`plugin "`+typ+`" (type `+typ+`): `+notAudited) {
		t.Errorf("refusal text %q", err)
	}
	if ran {
		t.Fatal("the unsupported plugin's constructor ran before the refusal")
	}
	cfg.Allow = func(string) (bool, string) { return true, "" }
	if _, err := build(t, cfg); !errors.As(err, &re) || re.Reason != notAudited {
		t.Fatalf("an unaudited type the host allows must still be refused as not audited, got %v", err)
	}
}

// TestRefusalPrecedence: a type the fork cannot host is refused with its own reason before the
// host is asked; a type the host refuses is refused with the host's reason; an audited type the
// host allows runs.
func TestRefusalPrecedence(t *testing.T) {
	asked := map[string]bool{}
	p := policy{allow: func(typ string) (bool, string) {
		asked[typ] = true
		if typ == "queue-scorer" || typ == "session-affinity-scorer" {
			return false, "host says no"
		}
		return true, ""
	}}
	for _, tc := range []struct{ typ, reason string }{
		{"redis-syncer", "it synchronizes through an external Redis"},
		{"queue-scorer", "host says no"},
		{"session-affinity-scorer", "host says no"},
		{"mm-embeddings-cache-producer", notAudited},
		{"max-score-picker", ""},
	} {
		reason, ok := p.refusal(tc.typ)
		if reason != tc.reason || ok != (tc.reason == "") {
			t.Errorf("refusal(%s) = %q, %v; want %q", tc.typ, reason, ok, tc.reason)
		}
	}
	if asked["redis-syncer"] {
		t.Error("the host was asked about a type the fork cannot host")
	}
	for typ := range hostingReasons {
		if audited[typ] {
			t.Errorf("%s is both audited and refused by the fork", typ)
		}
	}
}

// TestUnsupportedPluginReachedThroughFlowControlIsRefused: a plugin the configuration reaches only
// through a flow-control reference (llm-d's deploy/config/sim-program-aware-config.yaml) is
// refused at startup, never run unconverted.
func TestUnsupportedPluginReachedThroughFlowControlIsRefused(t *testing.T) {
	config := "plugins:\n- type: program-aware-fairness\n- type: queue-scorer\n- type: max-score-picker\n- type: single-profile-handler\n" +
		"featureGates:\n- flowControl\nflowControl:\n  defaultPriorityBand:\n    fairnessPolicyRef: program-aware-fairness\n" +
		"schedulingProfiles:\n- name: default\n  plugins:\n  - pluginRef: queue-scorer\n  - pluginRef: max-score-picker\n"
	_, err := build(t, withConfig(config))
	var re *contract.RefusalError
	if !errors.As(err, &re) || re.Type != "program-aware-fairness" {
		t.Fatalf("want program-aware-fairness refused, got %v", err)
	}
	ref := strings.Replace(config, "- type: program-aware-fairness\n", "", 1) // reached through the ref alone
	if _, err := build(t, withConfig(ref)); err == nil || !strings.Contains(err.Error(), "program-aware-fairness") {
		t.Fatalf("want program-aware-fairness refused when only the flow-control ref names it, got %v", err)
	}
}

// TestFactoriesRestored: a refusal policy does not outlive the construction it guarded.
func TestFactoriesRestored(t *testing.T) {
	cfg := withConfig("")
	cfg.Allow = func(typ string) (bool, string) { return typ != "queue-scorer", "no" }
	if _, err := build(t, cfg); err == nil {
		t.Fatal("the default configuration has a queue-scorer; want it refused")
	}
	if _, err := build(t, withConfig("")); err != nil {
		t.Fatalf("a later router without a policy must construct: %v", err)
	}
}

// TestNewRefusals: each refusal New makes itself, with its text, in the order New documents.
func TestNewRefusals(t *testing.T) {
	pd := "plugins:\n- type: token-producer\n  parameters:\n    modelName: m\n    vllm:\n      endpointDiscovery:\n        discoverModelLimits: true\n"
	precise := func(index string) string {
		return "plugins:\n- type: precise-prefix-cache-producer\n  parameters:\n    indexerConfig:\n      kvBlockIndexConfig:\n        " + index + "\n"
	}
	for _, tc := range []struct {
		name string
		cfg  contract.Config
		want string
	}{
		{"bad flag", contract.Config{Flags: []string{"--no-such-flag"}}, "EPP flags: unknown flag: --no-such-flag"},
		{"stray argument", contract.Config{Flags: append([]string{"stray"}, defaultFlags...)}, `EPP flags: unexpected arguments ["stray"]`},
		{"config file", contract.Config{Flags: append([]string{"--config-file=/nonexistent/x.yaml"}, defaultFlags...)}, "EPP flag --config-file: open /nonexistent/x.yaml: no such file or directory"},
		{"no pool", contract.Config{Flags: []string{"--pool-name=nope"}}, "--pool-name=nope: no InferencePool default/nope in the manifests"},
		{"pool with selector", contract.Config{Flags: defaultFlags, Manifests: []byte(testPool)},
			"the manifests define an InferencePool but the EPP flags select a standalone pool (--endpoint-selector); pass --pool-name"},
		{"pod manifest", contract.Config{Flags: defaultFlags, Manifests: []byte("apiVersion: v1\nkind: Pod\nmetadata:\n  name: p\n")},
			"manifest document 0: a Pod manifest cannot be simulated: BLIS creates the model-server pods from the scenario"},
		{"endpoint discovery", withConfig(pd), refusedPrefix + `plugin "token-producer" discovers render endpoints (vllm.endpointDiscovery)`},
		{"redis index", withConfig(precise("redisConfig:\n          address: x")), refusedPrefix + `plugin "precise-prefix-cache-producer" keeps its index in Redis, an external service`},
		{"cost-aware index", withConfig(precise("costAwareMemoryConfig:\n          size: 1GiB")), refusedPrefix + `plugin "precise-prefix-cache-producer" uses the cost-aware index`},
		{"peer topology", withConfig("plugins:\n- type: topology-extractor\n- type: topology-affinity-filter\n  parameters:\n    peerTopologyHeader: x\n"),
			refusedPrefix + "topology-affinity-filter peerTopologyHeader requires a coordinator; BLIS embeds one EPP"},
		{"topology without extractor", withConfig("plugins:\n- type: topology-affinity-filter\n"),
			refusedPrefix + `topology-affinity-filter needs topology-extractor "topology-extractor"`},
		{"malformed config", withConfig("[:"), "llm-d router: failed to parse configuration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := build(t, tc.cfg)
			if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error starting %q", err, tc.want)
			}
		})
	}
}

// TestCheckRunsFirstAndUnwrapped: the host check sees the Requirements and Pool before any
// hosting refusal or plugin is built, and its error is returned as is.
func TestCheckRunsFirstAndUnwrapped(t *testing.T) {
	hostErr := errors.New("host refuses")
	cfg := withConfig("plugins:\n- type: precise-prefix-cache-producer\n  parameters:\n    tokenProcessorConfig:\n      blockSizeTokens: 64\n" +
		"    indexerConfig:\n      kvBlockIndexConfig:\n        redisConfig:\n          address: x\n")
	var got contract.Requirements
	cfg.Check = func(req contract.Requirements, _ contract.Pool) error { got = req; return hostErr }
	cfg.Allow = func(string) (bool, string) { t.Error("Allow consulted before Check"); return true, "" }
	if _, err := build(t, cfg); err != hostErr { //nolint:errorlint // the contract requires it unwrapped
		t.Fatalf("got %v, want the host's error unwrapped", err)
	}
	if len(got.PrefixIndexes) != 1 || got.PrefixIndexes[0].BlockSize != 64 {
		t.Errorf("Check saw PrefixIndexes %+v", got.PrefixIndexes)
	}
}

// TestRequirements: the facts of each configuration form a host checks, with the router's
// defaults applied.
func TestRequirements(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags []string
		cfg   string
		want  contract.Requirements
	}{
		{"defaults", nil, "", contract.Requirements{Extractors: []contract.MetricsExtractor{
			{Name: "core-metrics-extractor", Implicit: true, EngineLabelKey: "llm-d.ai/engine-type", DefaultEngine: "vllm"}}}},
		{"inert flags", []string{"--tracing=true", "--ha-enable-leader-election"}, "", contract.Requirements{
			InertFlags: []string{"--ha-enable-leader-election", "--tracing"},
			Extractors: []contract.MetricsExtractor{{Name: "core-metrics-extractor", Implicit: true, EngineLabelKey: "llm-d.ai/engine-type", DefaultEngine: "vllm"}}}},
		{"extractor, source, index, topology, disagg", nil, `plugins:
- type: core-metrics-extractor
  name: x
  parameters:
    engineLabelKey: example.com/engine
    defaultEngine: sglang
    engineConfigs:
    - name: vllm
      queuedRequestsSpec: vllm:num_requests_waiting
      customMetrics:
      - {attributeKey: p, metricSpec: vllm:a, optional: true}
      - {attributeKey: q, metricSpec: vllm:b}
- type: metrics-data-source
  parameters:
    path: /stats
- type: metrics-data-source
  name: m2
- type: precise-prefix-cache-producer
  parameters:
    tokenProcessorConfig:
      blockSize: 32
    kvEventsConfig:
      engineType: sglang
- type: topology-extractor
  parameters:
    zone: example.com/zone
- type: disagg-profile-handler
`, contract.Requirements{
			Extractors: []contract.MetricsExtractor{{Name: "x", EngineLabelKey: "example.com/engine", DefaultEngine: "sglang",
				Engines: []contract.EngineMapping{{Name: "vllm", Series: map[string]string{"queuedRequestsSpec": "vllm:num_requests_waiting"},
					Custom: []contract.CustomMetric{{Series: "vllm:a", Optional: true}, {Series: "vllm:b"}}}}}},
			MetricsSources: []contract.MetricsSource{{Name: "metrics-data-source", Path: "/stats"}, {Name: "m2", Path: "/metrics"}},
			PrefixIndexes:  []contract.PrefixIndex{{Name: "precise-prefix-cache-producer", BlockSize: 32, EngineType: "sglang"}},
			Topology: contract.Topology{Requested: true, Extractors: []contract.TopologyExtractor{{Name: "topology-extractor",
				Hostname: "kubernetes.io/hostname", Rack: "topology.kubernetes.io/rack", Zone: "example.com/zone", Region: "topology.kubernetes.io/region"}}},
			Disaggregates: true,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			flags, err := parseFlags(append(tc.flags, defaultFlags...))
			if err != nil {
				t.Fatal(err)
			}
			if got := requirements(flags, readConfig([]byte(tc.cfg))); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("requirements =\n  %+v\nwant\n  %+v", got, tc.want)
			}
		})
	}
	flags, err := parseFlags(append([]string{"--total-queued-requests-metric=x"}, defaultFlags...))
	if err == nil {
		if got := requirements(flags, readConfig(nil)).MetricFlags; got["total-queued-requests-metric"] != "x" {
			t.Errorf("MetricFlags = %v", got)
		}
	} else if !strings.HasPrefix(err.Error(), "EPP flags: ") {
		t.Errorf("a legacy metric flag: %v", err)
	}
}

// TestPoolAndFlowControl: the pool's identity for both pool forms, and flow control's budget.
func TestPoolAndFlowControl(t *testing.T) {
	r, err := build(t, withConfig(""))
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Pool().ID(); got != "Deployment.apps default/epp" {
		t.Errorf("standalone pool ID %q", got)
	}
	r, err = build(t, poolConfig(holdConfig))
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Pool().ID(); got != "InferencePool.inference.networking.k8s.io default/vllm" {
		t.Errorf("InferencePool ID %q", got)
	}
	if got, want := r.FlowControl(), (contract.FlowControl{Enabled: true, MaxQueueTTL: 2 * time.Second, ExpirySweep: time.Second}); got != want {
		t.Errorf("FlowControl = %+v, want %+v", got, want)
	}
	if !r.IdleSkippable() {
		t.Error("the hold configuration has no loop counting its own ticks")
	}
}
