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

// Decision tests of the reference configurations (reference-configs/): each file is given to the
// hosted router and requests are routed through its real scheduling path.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inference-sim/blis-substrate/contracts/conformance/embedtest"
	contract "github.com/inference-sim/blis-substrate/contracts/embed"
	"sigs.k8s.io/yaml"
)

func refConfig(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "reference-configs", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// refPool is a hosted router with endpoints: prefill pods 10.0.1.n and decode pods 10.0.2.n
// carry the role label, plain pods 10.0.3.n carry none.
type refPool struct {
	t  *testing.T
	rt *embedtest.Runtime
	r  contract.Router
	n  int
}

// newRefPool starts a router with the configuration cfg (the router's built-in default when
// empty). queue gives the waiting-queue length an endpoint reports, by IP.
func newRefPool(t *testing.T, cfg string, prefill, decode, plain int, queue map[string]int) *refPool {
	t.Helper()
	return newRefPoolWithFlags(t, defaultFlags, cfg, prefill, decode, plain, queue)
}

// newRefPoolWithFlags is newRefPool with the EPP flags the file under test needs.
func newRefPoolWithFlags(t *testing.T, flags []string, cfg string, prefill, decode, plain int, queue map[string]int) *refPool {
	t.Helper()
	rt := embedtest.NewRuntime(1)
	rt.Engines = func(r *http.Request) (*http.Response, error) {
		body := fmt.Sprintf("# TYPE vllm:num_requests_waiting gauge\nvllm:num_requests_waiting{model_name=\"m\"} %d\n", queue[r.URL.Hostname()])
		return embedtest.Respond(r, http.StatusOK, "text/plain", body), nil
	}
	r, err := New(context.Background(), rt, contract.Config{Flags: flags, Config: []byte(cfg)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Stop)
	add := func(ip, role string) {
		labels := map[string]string{"llm-d.ai/inference-serving": "true"}
		if role != "" {
			labels["llm-d.ai/role"] = role
		}
		if err := r.SetEndpoint(contract.Endpoint{Name: ip, IP: ip, Port: 8000, Labels: labels}, true); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= prefill; i++ {
		add(fmt.Sprintf("10.0.1.%d", i), "prefill")
	}
	for i := 1; i <= decode; i++ {
		add(fmt.Sprintf("10.0.2.%d", i), "decode")
	}
	for i := 1; i <= plain; i++ {
		add(fmt.Sprintf("10.0.3.%d", i), "")
	}
	rt.Settle()
	rt.Advance(200 * time.Millisecond)
	return &refPool{t: t, rt: rt, r: r}
}

// route sends one completions request with the given prompt token ids and returns the decision.
// Unless hold is set the request completes at once, so the router counts it no longer in flight.
func (p *refPool) route(prompt []int, hold bool) (contract.Decision, contract.Flight) {
	p.t.Helper()
	d, fl, rej := p.tryRoute(prompt, hold)
	if rej != nil {
		p.t.Fatalf("request %d: rejection=%+v", p.n, rej)
	}
	return d, fl
}

// tryRoute is route that returns the rejection instead of failing on it.
func (p *refPool) tryRoute(prompt []int, hold bool) (contract.Decision, contract.Flight, *contract.Rejection) {
	p.t.Helper()
	p.n++
	body, _ := json.Marshal(map[string]any{"model": "m", "prompt": prompt, "max_tokens": 4})
	var d contract.Decision
	var fl contract.Flight
	var rej *contract.Rejection
	decided := false
	p.r.Route(context.Background(), contract.Request{ID: fmt.Sprintf("req-%d", p.n), Path: "/v1/completions", Body: body},
		func(f contract.Flight, dd contract.Decision, rr *contract.Rejection) {
			decided, fl, d, rej = true, f, dd, rr
		})
	p.rt.Settle()
	p.rt.Advance(0)
	if !decided {
		p.t.Fatalf("request %d was never decided", p.n)
	}
	if rej == nil && !hold {
		fl.ResponseBody([]byte(`{}`), true)
		p.rt.Settle()
	}
	return d, fl, rej
}

var shortPrompt = []int{1, 2, 3}

// longPrompt is a prompt of n tokens that differs from any other seed's.
func longPrompt(seed, n int) []int {
	p := make([]int, n)
	for i := range p {
		p[i] = seed*100000 + i
	}
	return p
}

// isCycle reports whether seq repeats its first k entries, with those k all different.
func isCycle(seq []string, k int) bool {
	first := map[string]bool{}
	for _, s := range seq[:k] {
		first[s] = true
	}
	if len(first) != k {
		return false
	}
	for i := k; i < len(seq); i++ {
		if seq[i] != seq[i%k] {
			return false
		}
	}
	return true
}

func counts(seq []string) map[string]int {
	m := map[string]int{}
	for _, s := range seq {
		m[s]++
	}
	return m
}

// TestRoundRobinOfAnEvenPDPoolReachesEveryEndpointButIsNotStrict states what the round-robin P/D
// file does in a hosted router. The max-score picker draws one counter at every profile pick, so
// with a prefill and a decode pick per request one might expect an even-sized pool to see only
// every other endpoint. It does not: the candidates reach the picker in a seeded random order
// (scheduler_profile.go), so rotating them by any counter leaves the choice uniformly random.
// Every endpoint of every pool is used, and the choice is not a fixed cycle. It is not strict
// per-request round-robin.
func TestRoundRobinOfAnEvenPDPoolReachesEveryEndpointButIsNotStrict(t *testing.T) {
	for _, size := range []int{2, 4, 3} { // two even pools and an odd one
		t.Run(fmt.Sprintf("%dP%dD", size, size), func(t *testing.T) {
			p := newRefPool(t, refConfig(t, "round-robin-pd.yaml"), size, size, 0, nil)
			const requests = 240
			var prefillSeq, decodeSeq []string
			for i := 0; i < requests; i++ {
				d, _ := p.route(shortPrompt, false)
				if !strings.HasPrefix(d.Prefill, "10.0.1.") || !strings.HasPrefix(d.Endpoint, "10.0.2.") {
					t.Fatalf("request %d went to prefill %q and decode %q; roles are not respected", i, d.Prefill, d.Endpoint)
				}
				prefillSeq, decodeSeq = append(prefillSeq, d.Prefill), append(decodeSeq, d.Endpoint)
			}
			for pool, seq := range map[string][]string{"prefill": prefillSeq, "decode": decodeSeq} {
				c := counts(seq)
				if len(c) != size {
					t.Errorf("%s pool of %d: only %d endpoints were ever chosen: %v", pool, size, len(c), c)
				}
				for ep, n := range c {
					if n < requests/size/2 {
						t.Errorf("%s pool of %d: %s chosen %d of %d times, far below an equal share", pool, size, ep, n, requests)
					}
				}
				if isCycle(seq, size) {
					t.Errorf("%s pool of %d: the choice is a fixed cycle; the index says it is not", pool, size)
				}
				t.Logf("%s pool of %d: counts %v", pool, size, c)
			}
		})
	}
}

// TestRoundRobinOfAColocatedPoolIsNotStrict: the same holds without disaggregation.
func TestRoundRobinOfAColocatedPoolIsNotStrict(t *testing.T) {
	p := newRefPool(t, refConfig(t, "round-robin.yaml"), 0, 0, 4, nil)
	var seq []string
	for i := 0; i < 200; i++ {
		d, _ := p.route(shortPrompt, false)
		seq = append(seq, d.Endpoint)
	}
	c := counts(seq)
	if len(c) != 4 {
		t.Errorf("only %d of 4 endpoints were chosen: %v", len(c), c)
	}
	if isCycle(seq, 4) {
		t.Error("the choice is a fixed cycle; the index says it is not")
	}
}

// TestRandomPickerFilesUseEveryEndpointOfTheirRole: the random-picker files choose among the
// endpoints their filters leave, in P/D and colocated.
func TestRandomPickerFilesUseEveryEndpointOfTheirRole(t *testing.T) {
	pd := newRefPool(t, refConfig(t, "random-picker-pd.yaml"), 3, 3, 0, nil)
	var prefill, decode []string
	for i := 0; i < 150; i++ {
		d, _ := pd.route(shortPrompt, false)
		prefill, decode = append(prefill, d.Prefill), append(decode, d.Endpoint)
	}
	for _, ep := range prefill {
		if !strings.HasPrefix(ep, "10.0.1.") {
			t.Fatalf("a prefill pick landed on %s", ep)
		}
	}
	for _, ep := range decode {
		if !strings.HasPrefix(ep, "10.0.2.") {
			t.Fatalf("a decode pick landed on %s", ep)
		}
	}
	if len(counts(prefill)) != 3 || len(counts(decode)) != 3 {
		t.Errorf("not every endpoint was chosen: prefill %v decode %v", counts(prefill), counts(decode))
	}

	colo := newRefPool(t, refConfig(t, "random-picker.yaml"), 0, 0, 4, nil)
	var seq []string
	for i := 0; i < 150; i++ {
		d, _ := colo.route(shortPrompt, false)
		seq = append(seq, d.Endpoint)
	}
	if len(counts(seq)) != 4 {
		t.Errorf("not every colocated endpoint was chosen: %v", counts(seq))
	}
}

// TestLeastActiveRequestsPicksTheIdleEndpoint: with requests held in flight, each new request
// goes to an endpoint that has none, and when one finishes the next goes to it. In P/D the
// prefill and the decode pool are each balanced by their own counts.
func TestLeastActiveRequestsPicksTheIdleEndpoint(t *testing.T) {
	t.Run("colocated", func(t *testing.T) {
		p := newRefPool(t, refConfig(t, "least-active-requests.yaml"), 0, 0, 4, nil)
		seen := map[string]contract.Flight{}
		var order []string
		for i := 0; i < 4; i++ {
			d, f := p.route(shortPrompt, true)
			if seen[d.Endpoint] != nil {
				t.Fatalf("request %d went to %s, which already had a request in flight; held %v", i, d.Endpoint, order)
			}
			seen[d.Endpoint] = f
			order = append(order, d.Endpoint)
		}
		freed := order[2]
		seen[freed].ResponseBody([]byte(`{}`), true)
		p.rt.Settle()
		if d, _ := p.route(shortPrompt, true); d.Endpoint != freed {
			t.Errorf("after %s finished, the next request went to %s", freed, d.Endpoint)
		}
	})
	t.Run("pd", func(t *testing.T) {
		p := newRefPool(t, refConfig(t, "least-active-requests-pd.yaml"), 3, 3, 0, nil)
		prefillHeld, decodeHeld := map[string]bool{}, map[string]bool{}
		var flights []contract.Flight
		var decisions []contract.Decision
		for i := 0; i < 3; i++ {
			d, f := p.route(shortPrompt, true)
			if prefillHeld[d.Prefill] || decodeHeld[d.Endpoint] {
				t.Fatalf("request %d reused an endpoint that had a request in flight: prefill %s decode %s", i, d.Prefill, d.Endpoint)
			}
			prefillHeld[d.Prefill], decodeHeld[d.Endpoint] = true, true
			flights, decisions = append(flights, f), append(decisions, d)
		}
		flights[1].ResponseBody([]byte(`{}`), true)
		p.rt.Settle()
		// The two pools had one request each on all three endpoints; the finished request freed
		// one endpoint in each.
		d, _ := p.route(shortPrompt, true)
		if d.Prefill != decisions[1].Prefill || d.Endpoint != decisions[1].Endpoint {
			t.Errorf("after request 1 finished, the next went to prefill %s decode %s, want %s and %s",
				d.Prefill, d.Endpoint, decisions[1].Prefill, decisions[1].Endpoint)
		}
	})
}

// builtInAndFile runs fn for a router given no configuration and for one given default.yaml.
func builtInAndFile(t *testing.T, fn func(t *testing.T, cfg string)) {
	t.Run("built-in", func(t *testing.T) { fn(t, "") })
	t.Run("default.yaml", func(t *testing.T) { fn(t, refConfig(t, "default.yaml")) })
}

// TestDefaultAvoidsALongQueue: the default policy scores by queue length (weight 2), so an
// endpoint that reports a long queue is not chosen while others are idle.
func TestDefaultAvoidsALongQueue(t *testing.T) {
	builtInAndFile(t, func(t *testing.T, cfg string) {
		p := newRefPool(t, cfg, 0, 0, 4, map[string]int{"10.0.3.1": 20})
		for i := 0; i < 24; i++ {
			if d, _ := p.route(longPrompt(i, 300), false); d.Endpoint == "10.0.3.1:8000" {
				t.Fatalf("request %d went to the endpoint with a queue of 20", i)
			}
		}
	})
}

// TestDefaultFollowsAPrefix: the default policy scores by cached prefix (weight 3), so a prompt
// sent twice goes to the same endpoint both times although the others are idle.
func TestDefaultFollowsAPrefix(t *testing.T) {
	builtInAndFile(t, func(t *testing.T, cfg string) {
		p := newRefPool(t, cfg, 0, 0, 4, nil)
		for i := 0; i < 12; i++ {
			prompt := longPrompt(i, 300)
			first, _ := p.route(prompt, false)
			second, _ := p.route(prompt, false)
			if first.Endpoint != second.Endpoint {
				t.Fatalf("prompt %d went to %s and then to %s", i, first.Endpoint, second.Endpoint)
			}
		}
	})
}

// TestDefaultHasNoPDForm: in a pool that mixes roles the default policy names no prefill
// endpoint and chooses among all endpoints, prefill-role ones included.
func TestDefaultHasNoPDForm(t *testing.T) {
	builtInAndFile(t, func(t *testing.T, cfg string) {
		p := newRefPool(t, cfg, 2, 2, 0, nil)
		onPrefillPod := false
		for i := 0; i < 24; i++ {
			d, _ := p.route(longPrompt(i, 300), false)
			if d.Prefill != "" {
				t.Fatalf("request %d named prefill endpoint %s", i, d.Prefill)
			}
			onPrefillPod = onPrefillPod || strings.HasPrefix(d.Endpoint, "10.0.1.")
		}
		if !onPrefillPod {
			t.Error("no request reached a prefill-role pod; the index says the default has no role filter")
		}
	})
}

// withParameter returns the configuration text with a plugin parameter set.
func withParameter(t *testing.T, text, pluginType, key string, value any) string {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		t.Fatal(err)
	}
	set := false
	for _, pl := range doc["plugins"].([]any) {
		m := pl.(map[string]any)
		if m["type"] == pluginType {
			params, _ := m["parameters"].(map[string]any)
			if params == nil {
				params = map[string]any{}
				m["parameters"] = params
			}
			params[key], set = value, true
		}
	}
	if !set {
		t.Fatalf("no %s plugin in the configuration", pluginType)
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// TestPDSampleDisaggregatesByUncachedTokens: the router's P/D sample sends a request to a
// prefill endpoint when at least nonCachedTokens (16) of its prompt are not cached on the decode
// endpoint, and not otherwise. nonCachedTokens is a knob: raised above the prompt, nothing
// disaggregates.
func TestPDSampleDisaggregatesByUncachedTokens(t *testing.T) {
	t.Run("as the sample states", func(t *testing.T) {
		p := newRefPool(t, refConfig(t, "pd-prefix-based.yaml"), 2, 2, 0, nil)
		long := longPrompt(1, 600)
		d, _ := p.route(long, false)
		if !strings.HasPrefix(d.Prefill, "10.0.1.") || !strings.HasPrefix(d.Endpoint, "10.0.2.") {
			t.Fatalf("a new 600-token prompt: prefill %q decode %q, want a prefill and a decode endpoint", d.Prefill, d.Endpoint)
		}
		again, _ := p.route(long, false)
		if again.Prefill != "" || again.Endpoint != d.Endpoint {
			t.Errorf("the same prompt again: prefill %q decode %q, want no prefill (cached) on %s", again.Prefill, again.Endpoint, d.Endpoint)
		}
		if short, _ := p.route([]int{7, 8, 9, 10, 11}, false); short.Prefill != "" {
			t.Errorf("a 5-token prompt (under nonCachedTokens 16) was sent to prefill endpoint %s", short.Prefill)
		}
	})
	t.Run("nonCachedTokens raised", func(t *testing.T) {
		cfg := withParameter(t, refConfig(t, "pd-prefix-based.yaml"), "prefix-based-pd-decider", "nonCachedTokens", 100000)
		p := newRefPool(t, cfg, 2, 2, 0, nil)
		for i := 0; i < 5; i++ {
			if d, _ := p.route(longPrompt(i, 600), false); d.Prefill != "" {
				t.Fatalf("request %d disaggregated with nonCachedTokens above its length", i)
			}
		}
	})
}
