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
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/inference-sim/blis-substrate/contracts/conformance/embedtest"
	contract "github.com/inference-sim/blis-substrate/contracts/embed"
)

// experimentalFlags select the standalone pool and allow the Alpha plugins the llm-d guides use.
var experimentalFlags = append([]string{"--allow-experimental-plugins"}, defaultFlags...)

// utilizationConfig is a colocated profile that filters with a utilization-filter and then picks
// at random, so the filter alone decides which endpoints can be chosen.
func utilizationConfig(conditions string, fallback bool) string {
	return fmt.Sprintf(`apiVersion: llm-d.ai/v1
kind: EndpointPickerConfig
plugins:
- type: inflight-load-producer
- type: utilization-filter
  name: guard
  parameters:
    conditions:
%s
    fallbackOnEmpty: %v
- type: random-picker
- type: single-profile-handler
schedulingProfiles:
- name: default
  plugins:
  - pluginRef: guard
  - pluginRef: random-picker
`, conditions, fallback)
}

const waitingAtMost2 = "    - metric: waiting-queue\n      maxValue: 2"

// chosen routes n requests (each completed at once) and counts the endpoints chosen.
func chosen(p *refPool, n int) map[string]int {
	seq := make([]string, 0, n)
	for i := 0; i < n; i++ {
		d, _ := p.route(shortPrompt, false)
		seq = append(seq, d.Endpoint)
	}
	return counts(seq)
}

// TestUtilizationFilterKeepsRequestsOffAnOverloadedEndpoint: a scraped waiting queue above the cap
// removes the endpoint from every choice; a queue exactly at the cap is kept (maxValue is
// inclusive); without the filter the overloaded endpoint is chosen like any other.
func TestUtilizationFilterKeepsRequestsOffAnOverloadedEndpoint(t *testing.T) {
	queue := map[string]int{"10.0.3.1": 3, "10.0.3.2": 2, "10.0.3.3": 0}
	p := newRefPoolWithFlags(t, experimentalFlags, utilizationConfig(waitingAtMost2, false), 0, 0, 3, queue)
	got := chosen(p, 60)
	if got["10.0.3.1:8000"] != 0 {
		t.Errorf("the endpoint with waiting 3 was chosen %d times under waiting-queue maxValue 2: %v", got["10.0.3.1:8000"], got)
	}
	if got["10.0.3.2:8000"] == 0 || got["10.0.3.3:8000"] == 0 {
		t.Errorf("an endpoint within the cap was never chosen in 60 requests: %v", got)
	}

	// The control: the same pool and picker without the filter reaches the overloaded endpoint.
	control := strings.Replace(utilizationConfig(waitingAtMost2, false), "  - pluginRef: guard\n", "", 1)
	p = newRefPoolWithFlags(t, experimentalFlags, control, 0, 0, 3, queue)
	if got := chosen(p, 60); got["10.0.3.1:8000"] == 0 {
		t.Errorf("without the filter the overloaded endpoint was never chosen: %v", got)
	}
}

// TestUtilizationFilterFallbackOnEmpty: when every endpoint is over the cap the request is
// rejected, or with fallbackOnEmpty it is routed over the unfiltered list.
func TestUtilizationFilterFallbackOnEmpty(t *testing.T) {
	queue := map[string]int{"10.0.3.1": 5, "10.0.3.2": 7}
	p := newRefPoolWithFlags(t, experimentalFlags, utilizationConfig(waitingAtMost2, false), 0, 0, 2, queue)
	if d, _, rej := p.tryRoute(shortPrompt, false); rej == nil {
		t.Errorf("every endpoint is over the cap and fallbackOnEmpty is off, but the request was routed to %q", d.Endpoint)
	}
	p = newRefPoolWithFlags(t, experimentalFlags, utilizationConfig(waitingAtMost2, true), 0, 0, 2, queue)
	if got := chosen(p, 40); len(got) != 2 {
		t.Errorf("fallbackOnEmpty should route over both endpoints, got %v", got)
	}
}

// TestUtilizationFilterReadsTheInFlightCount: with active-requests maxValue 0, an endpoint that
// holds an unfinished request is skipped until the request completes.
func TestUtilizationFilterReadsTheInFlightCount(t *testing.T) {
	p := newRefPoolWithFlags(t, experimentalFlags, utilizationConfig("    - metric: active-requests\n      maxValue: 0", false), 0, 0, 2, nil)
	first, fl := p.route(shortPrompt, true)
	d, _ := p.route(shortPrompt, true)
	if d.Endpoint == first.Endpoint {
		t.Fatalf("the second request went to %s, which holds an unfinished request", d.Endpoint)
	}
	fl.ResponseBody([]byte(`{}`), true)
	p.rt.Settle()
	d, _ = p.route(shortPrompt, false)
	if d.Endpoint != first.Endpoint {
		t.Errorf("after the first request completed, the only endpoint under the cap is %s, got %s", first.Endpoint, d.Endpoint)
	}
}

// renderConfig is a colocated precise-prefix configuration whose token-producer renders prompts
// on url, or on the pool's endpoints when url is empty (the llm-d precise-prefix guide's form).
func renderConfig(url string) string {
	vllm := "      url: " + url
	if url == "" {
		vllm = "      endpointDiscovery:\n        attemptTimeout: 2s"
	}
	return `apiVersion: llm-d.ai/v1
kind: EndpointPickerConfig
plugins:
- type: token-producer
  parameters:
    modelName: m
    vllm:
` + vllm + `
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
}

// renderPool is a hosted router with plain endpoints 10.0.3.1..ready (and notReady more that are
// not Ready) whose engines answer metrics and render calls, and record every request they get.
type renderPool struct {
	*refPool
	calls []string // "host path" of each engine request, in order
}

func newRenderPool(t *testing.T, cfg string, ready, notReady int) *renderPool {
	t.Helper()
	rp := &renderPool{}
	rt := embedtest.NewRuntime(1)
	rt.Engines = func(r *http.Request) (*http.Response, error) {
		rp.calls = append(rp.calls, r.URL.Host+" "+r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/v1/completions/render") {
			return renderEngines(r)
		}
		return embedtest.Respond(r, http.StatusOK, "text/plain", "# TYPE vllm:num_requests_waiting gauge\nvllm:num_requests_waiting{model_name=\"m\"} 0\n"), nil
	}
	r, err := New(context.Background(), rt, contract.Config{Flags: defaultFlags, Config: []byte(cfg)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Stop)
	for i := 1; i <= ready+notReady; i++ {
		ip := fmt.Sprintf("10.0.3.%d", i)
		ep := contract.Endpoint{Name: ip, IP: ip, Port: 8000, Labels: map[string]string{"llm-d.ai/inference-serving": "true"}}
		if err := r.SetEndpoint(ep, i <= ready); err != nil {
			t.Fatal(err)
		}
	}
	rt.Settle()
	rt.Advance(200 * time.Millisecond)
	rp.refPool = &refPool{t: t, rt: rt, r: r}
	rp.calls = nil
	return rp
}

// renderHosts are the hosts the engines saw render calls on, in order.
func (p *renderPool) renderHosts() []string {
	var out []string
	for _, c := range p.calls {
		if host, path, _ := strings.Cut(c, " "); strings.HasSuffix(path, "/v1/completions/render") {
			out = append(out, host)
		}
	}
	return out
}

// TestTokenProducerEndpointDiscoveryRendersOnThePoolInRotation: with endpointDiscovery and no
// discoverModelLimits the token-producer renders on the pool's Ready endpoints in identity
// order, skips an endpoint that is not Ready, never probes /v1/models, and the routing decisions
// are those of the fixed-url form on the same endpoints. The host's assumption is that a render
// call takes no simulated time, which the engines' immediate answers state.
func TestTokenProducerEndpointDiscoveryRendersOnThePoolInRotation(t *testing.T) {
	const requests = 12
	discovered := newRenderPool(t, renderConfig(""), 4, 1)
	fixed := newRenderPool(t, renderConfig("http://render.embedtest.local:8000"), 4, 1)
	var got, want []string
	for i := 0; i < requests; i++ {
		prompt := longPrompt(i%3+1, 64)
		d, _ := discovered.route(prompt, false)
		f, _ := fixed.route(prompt, false)
		got, want = append(got, d.Endpoint), append(want, f.Endpoint)
	}
	hosts := discovered.renderHosts()
	if len(hosts) != requests {
		t.Fatalf("%d render calls for %d requests: %v", len(hosts), requests, hosts)
	}
	order := []string{"10.0.3.1:8000", "10.0.3.2:8000", "10.0.3.3:8000", "10.0.3.4:8000"}
	for i, h := range hosts {
		if h != order[i%len(order)] {
			t.Fatalf("render call %d went to %s, want %s (identity order over the Ready endpoints; the NotReady 10.0.3.5 is skipped): %v",
				i, h, order[i%len(order)], hosts)
		}
	}
	for _, c := range discovered.calls {
		if strings.Contains(c, "/v1/models") {
			t.Errorf("an engine was probed for its model limits: %s", c)
		}
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("decisions differ from the fixed-url form\n discovered %v\n fixed      %v", got, want)
	}
}

// TestEndpointDiscoveryRefusalIsTheModelLimitProbeOnly: discoverModelLimits is the setting that
// starts the 30 s probe loop; spelled false it is accepted like an absent one.
func TestEndpointDiscoveryRefusalIsTheModelLimitProbeOnly(t *testing.T) {
	off := strings.Replace(renderConfig(""), "attemptTimeout: 2s", "discoverModelLimits: false", 1)
	if _, err := build(t, contract.Config{Flags: defaultFlags, Config: []byte(off)}); err != nil {
		t.Errorf("discoverModelLimits: false is refused: %v", err)
	}
	on := strings.Replace(renderConfig(""), "attemptTimeout: 2s", "discoverModelLimits: true", 1)
	if _, err := build(t, contract.Config{Flags: defaultFlags, Config: []byte(on)}); err == nil ||
		!strings.Contains(err.Error(), "discoverModelLimits") {
		t.Errorf("discoverModelLimits: true must be refused naming the setting, got %v", err)
	}
}
