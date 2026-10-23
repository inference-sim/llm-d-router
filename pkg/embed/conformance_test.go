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
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	contract "github.com/inference-sim/blis-schemas/embed"
	"github.com/inference-sim/blis-schemas/embed/embedtest"
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

// TestConformance runs the blis-schemas embed conformance suite against New.
func TestConformance(t *testing.T) {
	evict := poolConfig(evictConfig)
	embedtest.Run(t, embedtest.Suite{
		New:        New,
		Labels:     map[string]string{"app": "vllm"},
		Engines:    renderEngines,
		Seed:       42,
		Observe:    200 * 1e6, // 200 ms: several metrics scrapes at the default 50 ms interval
		Route:      poolConfig(routeConfig),
		Hold:       poolConfig(holdConfig),
		KV:         poolConfig(kvConfig),
		Evict:      &evict,
		Refuse:     poolConfig(routeConfig),
		RefuseType: "queue-scorer",
	})
}
