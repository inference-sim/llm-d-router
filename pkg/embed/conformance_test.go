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
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/inference-sim/blis-substrate/contracts/conformance/embedtest"
	contract "github.com/inference-sim/blis-substrate/contracts/embed"

	errcommon "github.com/llm-d/llm-d-router/pkg/common/error"
	fwkplugin "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/plugin"
	fwksched "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/scheduling"
)

// testPool is an InferencePool selecting app=vllm, with an InferenceObjective named
// embedtest.Sheddable at a negative priority.
const testPool = `apiVersion: inference.networking.k8s.io/v1
kind: InferencePool
metadata:
  name: vllm
spec:
  selector:
    matchLabels:
      app: vllm
  targetPorts:
  - number: 8000
  endpointPickerRef:
    name: epp
    port:
      number: 9002
---
apiVersion: llm-d.ai/v1
kind: InferenceObjective
metadata:
  name: ` + embedtest.Sheddable + `
spec:
  priority: -2
  poolRefs:
  - name: vllm
`

const routeConfig = `apiVersion: llm-d.ai/v1
kind: EndpointPickerConfig
plugins:
- type: queue-scorer
- type: max-score-picker
- type: single-profile-handler
schedulingProfiles:
- name: default
  plugins:
  - pluginRef: queue-scorer
  - pluginRef: max-score-picker
`

const holdConfig = `apiVersion: llm-d.ai/v1
kind: EndpointPickerConfig
featureGates:
- flowControl
plugins:
- type: queue-scorer
- type: max-score-picker
- type: single-profile-handler
schedulingProfiles:
- name: default
  plugins:
  - pluginRef: queue-scorer
  - pluginRef: max-score-picker
flowControl:
  maxRequests: 1
  defaultRequestTTL: 2s
`

const evictConfig = `apiVersion: llm-d.ai/v1
kind: EndpointPickerConfig
featureGates:
- flowControl
plugins:
- type: queue-scorer
- type: max-score-picker
- type: single-profile-handler
schedulingProfiles:
- name: default
  plugins:
  - pluginRef: max-score-picker
  - pluginRef: queue-scorer
flowControl:
  enableEviction: true
`

// kvConfig is llm-d's deploy/config/sim-epp-kvcache-config.yaml with its model and a render URL
// the suite's engines answer.
const kvConfig = `apiVersion: llm-d.ai/v1
kind: EndpointPickerConfig
plugins:
- type: token-producer
  parameters:
    modelName: test-model
    vllm:
      url: http://render.embedtest.local:8000
- type: precise-prefix-cache-producer
  parameters:
    tokenProcessorConfig:
      blockSizeTokens: 16
    kvEventsConfig:
      zmqEndpoint: tcp://0.0.0.0:5557
    indexerConfig:
      kvBlockIndexConfig:
        enableMetrics: false
- type: prefix-cache-scorer
  parameters:
    prefixMatchInfoProducerName: precise-prefix-cache-producer
- type: max-score-picker
- type: single-profile-handler
schedulingProfiles:
- name: default
  plugins:
  - pluginRef: max-score-picker
  - pluginRef: prefix-cache-scorer
    weight: 10
`

// unauditedConfig is routeConfig plus header-label-affinity-scorer, a plugin type the fork has
// not audited for hosting (refusal step 3: refused even when the host allows it).
var unauditedConfig = strings.Replace(routeConfig, "- type: queue-scorer\n", "- type: queue-scorer\n- type: header-label-affinity-scorer\n", 1)

func poolConfig(epc string) contract.Config {
	return contract.Config{Flags: []string{"--pool-name=vllm"}, Config: []byte(epc), Manifests: []byte(testPool)}
}

// renderEngines answers vLLM's /v1/completions/render for a token-ID prompt: the rendered prompt
// is the token IDs themselves.
func renderEngines(r *http.Request) (*http.Response, error) {
	if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/v1/completions/render") {
		return embedtest.Respond(r, http.StatusNotFound, "text/plain", "not served\n"), nil
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	var req struct {
		Prompt json.RawMessage `json:"prompt"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return embedtest.Respond(r, http.StatusBadRequest, "text/plain", err.Error()), nil
	}
	var single []uint32
	var batch [][]uint32
	switch {
	case json.Unmarshal(req.Prompt, &single) == nil:
		batch = [][]uint32{single}
	case json.Unmarshal(req.Prompt, &batch) == nil:
	default:
		return embedtest.Respond(r, http.StatusNotImplemented, "text/plain", "token-ID prompts only\n"), nil
	}
	type rendered struct {
		TokenIDs []uint32 `json:"token_ids"`
	}
	out := make([]rendered, len(batch))
	for i, ids := range batch {
		out[i] = rendered{TokenIDs: ids}
	}
	b, _ := json.Marshal(out)
	return embedtest.Respond(r, http.StatusOK, "application/json", string(b)), nil
}

// unavailableType is a test-only admitter that refuses every request with ServiceUnavailable and
// no drop reason: the one outcome the router's own plugins reach only on a path the suite cannot
// drive (GET /v1/models before any endpoint reports), so the Unavailable part needs a plugin
// that makes it routable.
const unavailableType = "embed-test-unavailable-admitter"

type unavailableAdmitter struct{ name string }

func (a unavailableAdmitter) TypedName() fwkplugin.TypedName {
	return fwkplugin.TypedName{Type: unavailableType, Name: a.name}
}

func (unavailableAdmitter) Admit(context.Context, *fwksched.InferenceRequest, []fwksched.Endpoint) error {
	return errcommon.Error{Code: errcommon.ServiceUnavailable, Msg: "no serving capacity"}
}

// registerUnavailable registers the admitter and marks it audited for the test's duration.
func registerUnavailable(t testing.TB) {
	t.Helper()
	fwkplugin.Register(unavailableType, fwkplugin.StabilityStable, func(name string, _ *json.Decoder, _ fwkplugin.Handle) (fwkplugin.Plugin, error) {
		return unavailableAdmitter{name}, nil
	})
	audited[unavailableType] = true
	t.Cleanup(func() {
		delete(fwkplugin.Registry, unavailableType)
		delete(fwkplugin.RegistryMetadata, unavailableType)
		delete(audited, unavailableType)
	})
}

var unavailableConfig = strings.Replace(routeConfig, "- type: queue-scorer\n", "- type: queue-scorer\n- type: "+unavailableType+"\n", 1)

// conformanceSuite is the suite for a router built by newRouter. It configures every part the
// fork relies on (Unavailable and Evict included), so none is skipped.
func conformanceSuite(newRouter contract.Constructor) embedtest.Suite {
	evict := poolConfig(evictConfig)
	unavailable := poolConfig(unavailableConfig)
	return embedtest.Suite{
		New:         newRouter,
		Labels:      map[string]string{"app": "vllm"},
		Engines:     renderEngines,
		Seed:        42,
		Observe:     200 * 1e6, // 200 ms: several metrics scrapes at the default 50 ms interval
		Route:       poolConfig(routeConfig),
		Hold:        poolConfig(holdConfig),
		KV:          poolConfig(kvConfig),
		Evict:       &evict,
		Unavailable: &unavailable,
		Refuse:      poolConfig(routeConfig),
		RefuseType:  "queue-scorer",

		Unaudited:       "header-label-affinity-scorer",
		UnauditedConfig: poolConfig(unauditedConfig),
	}
}

// TestConformance runs the blis-substrate embed conformance suite (contracts/conformance/embedtest) against New.
func TestConformance(t *testing.T) {
	registerUnavailable(t)
	s := conformanceSuite(New)
	if s.Unavailable == nil || s.Evict == nil {
		t.Fatal("the fork's conformance suite must configure Unavailable and Evict: a nil part is skipped, and BLIS relies on both")
	}
	embedtest.Run(t, s)
}

// misclassifying wraps a router so that every Unavailable rejection it makes is reported as
// Unclassified, the mutation the Unavailable part exists to catch.
type misclassifying struct{ contract.Router }

func (m misclassifying) Route(ctx context.Context, req contract.Request, done func(contract.Flight, contract.Decision, *contract.Rejection)) {
	m.Router.Route(ctx, req, func(f contract.Flight, d contract.Decision, rj *contract.Rejection) {
		if rj != nil && rj.Kind == contract.Unavailable {
			c := *rj
			c.Kind = contract.Unclassified
			rj = &c
		}
		done(f, d, rj)
	})
}

// TestConformanceCatchesMisclassifiedUnavailable: the suite passes the real router and fails one
// whose service-unavailable outcome is Unclassified, naming the unavailable part.
func TestConformanceCatchesMisclassifiedUnavailable(t *testing.T) {
	registerUnavailable(t)
	if err := embedtest.Check(conformanceSuite(New)); err != nil {
		t.Fatalf("the real router fails the suite: %v", err)
	}
	mutant := func(ctx context.Context, rt contract.Runtime, cfg contract.Config) (contract.Router, error) {
		r, err := New(ctx, rt, cfg)
		if err != nil {
			return nil, err
		}
		return misclassifying{r}, nil
	}
	err := embedtest.Check(conformanceSuite(mutant))
	if err == nil {
		t.Fatal("a router that reports Unavailable as Unclassified passes the suite")
	}
	if !strings.Contains(err.Error(), "unavailable") {
		t.Errorf("the suite failed, but not in its unavailable part: %v", err)
	}
}
