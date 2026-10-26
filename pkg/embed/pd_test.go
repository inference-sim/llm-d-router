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
	"net/http"
	"testing"
	"time"

	contract "github.com/inference-sim/blis-schemas/embed"
	"github.com/inference-sim/blis-schemas/embed/embedtest"
)

// pdConfig always disaggregates: the prefill profile picks a prefill pod, the decode profile a
// decode pod.
const pdConfig = `apiVersion: llm-d.ai/v1
kind: EndpointPickerConfig
plugins:
- type: queue-scorer
- type: prefill-filter
- type: decode-filter
- type: max-score-picker
- type: always-disagg-pd-decider
- type: disagg-profile-handler
  parameters:
    deciders:
      prefill: always-disagg-pd-decider
schedulingProfiles:
- name: prefill
  plugins:
  - pluginRef: prefill-filter
  - pluginRef: max-score-picker
  - pluginRef: queue-scorer
- name: decode
  plugins:
  - pluginRef: decode-filter
  - pluginRef: max-score-picker
  - pluginRef: queue-scorer
`

// TestDisaggregatedDecision: a disaggregating configuration reports Disaggregates, and its
// decision names the decode pod as Endpoint and the prefill pod the disaggregation profile
// handler chose (its prefill routing header) as Prefill.
func TestDisaggregatedDecision(t *testing.T) {
	rt := embedtest.NewRuntime(1)
	rt.Engines = func(r *http.Request) (*http.Response, error) {
		return embedtest.Respond(r, http.StatusOK, "text/plain", "# TYPE vllm:num_requests_waiting gauge\nvllm:num_requests_waiting{model_name=\"m\"} 0\n"), nil
	}
	cfg := withConfig(pdConfig)
	r, err := New(context.Background(), rt, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Stop()
	if !r.Requirements().Disaggregates {
		t.Error("Requirements().Disaggregates is false for a disagg-profile-handler configuration")
	}
	pods := map[string]string{"10.0.0.1": "prefill", "10.0.0.2": "decode"}
	for ip, role := range pods {
		ep := contract.Endpoint{Name: role, IP: ip, Port: 8000,
			Labels: map[string]string{"llm-d.ai/inference-serving": "true", "llm-d.ai/role": role}}
		if err := r.SetEndpoint(ep, true); err != nil {
			t.Fatal(err)
		}
	}
	rt.Settle()
	rt.Advance(200 * time.Millisecond)
	body, _ := json.Marshal(map[string]any{"model": "m", "prompt": []int{1, 2, 3}, "max_tokens": 4})
	var d contract.Decision
	var rej *contract.Rejection
	decided := false
	r.Route(context.Background(), contract.Request{ID: "r1", Path: "/v1/completions", Body: body}, func(_ contract.Flight, dd contract.Decision, rr *contract.Rejection) {
		decided, d, rej = true, dd, rr
	})
	rt.Settle()
	rt.Advance(0)
	if !decided || rej != nil {
		t.Fatalf("decided=%v rejection=%+v", decided, rej)
	}
	if d.Endpoint != "10.0.0.2:8000" || d.Prefill != "10.0.0.1:8000" {
		t.Errorf("decision Endpoint=%q Prefill=%q, want decode 10.0.0.2:8000 and prefill 10.0.0.1:8000", d.Endpoint, d.Prefill)
	}
}
