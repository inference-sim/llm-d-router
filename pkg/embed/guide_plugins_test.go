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
	"fmt"
	"strings"
	"testing"
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
