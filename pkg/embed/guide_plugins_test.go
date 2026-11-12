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
	"errors"
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

// screenerConfig is the plugin block of the llm-d wide-EP guide's router file with scoring
// reduced to the queue: the screener keeps one DisaggregatedSet revision per request, then the
// P/D profiles pick within it.
const screenerConfig = `apiVersion: llm-d.ai/v1
kind: EndpointPickerConfig
plugins:
- type: disaggregatedset-rollout-screener
  parameters:
    scope:
      labelSelector: "disaggregatedset.x-k8s.io/name=ws"
    revisionGating:
      mode: max-role
      requiredRoles: [prefill, decode]
- type: always-disagg-pd-decider
- type: disagg-profile-handler
  parameters:
    deciders:
      prefill: always-disagg-pd-decider
- type: prefill-filter
- type: decode-filter
- type: random-picker
schedulingProfiles:
- name: prefill
  plugins:
  - pluginRef: prefill-filter
  - pluginRef: random-picker
- name: decode
  plugins:
  - pluginRef: decode-filter
  - pluginRef: random-picker
`

// setPod adds or updates a pod of the DisaggregatedSet "ws". The third octet of its address
// says which revision and role it is: 10.0.<rev>.<n>, rev 1 and 2 prefill and decode of
// revision "a", 4 and 5 of revision "b".
func setPod(t *testing.T, r contract.Router, rev byte, n int, ready bool) {
	t.Helper()
	if err := trySetPod(r, rev, n, ready, true, true); err != nil {
		t.Fatal(err)
	}
}

func trySetPod(r contract.Router, rev byte, n int, ready, withRevision, withRole bool) error {
	revision, role := map[byte]string{1: "a", 2: "a", 4: "b", 5: "b"}[rev], map[byte]string{1: "prefill", 2: "decode", 4: "prefill", 5: "decode"}[rev]
	ip := fmt.Sprintf("10.0.%d.%d", rev, n)
	labels := map[string]string{
		"llm-d.ai/inference-serving":     "true",
		"llm-d.ai/role":                  role,
		"disaggregatedset.x-k8s.io/name": "ws",
	}
	if withRevision {
		labels["disaggregatedset.x-k8s.io/revision"] = revision
	}
	if withRole {
		labels["disaggregatedset.x-k8s.io/role"] = role
	}
	return r.SetEndpoint(contract.Endpoint{Name: ip, IP: ip, Port: 8000, Labels: labels}, ready)
}

// revisionOf reads the revision a decision went to from the pod addresses, and fails when the
// prefill and decode pick belong to different revisions.
func revisionOf(t *testing.T, d contract.Decision) string {
	t.Helper()
	rev := func(addr string) string {
		var a, b, c, e, port int
		if _, err := fmt.Sscanf(addr, "%d.%d.%d.%d:%d", &a, &b, &c, &e, &port); err != nil {
			t.Fatalf("address %q: %v", addr, err)
		}
		if c <= 2 {
			return "a"
		}
		return "b"
	}
	if rev(d.Prefill) != rev(d.Endpoint) {
		t.Fatalf("prefill %s and decode %s belong to different revisions", d.Prefill, d.Endpoint)
	}
	return rev(d.Endpoint)
}

func newScreenerPool(t *testing.T, seed int64) *refPool {
	t.Helper()
	rt := embedtest.NewRuntime(seed)
	rt.Engines = func(r *http.Request) (*http.Response, error) {
		return embedtest.Respond(r, http.StatusOK, "text/plain", "# TYPE vllm:num_requests_waiting gauge\nvllm:num_requests_waiting{model_name=\"m\"} 0\n"), nil
	}
	r, err := New(context.Background(), rt, contract.Config{Flags: experimentalFlags, Config: []byte(screenerConfig)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Stop)
	return &refPool{t: t, rt: rt, r: r}
}

// share routes n requests and returns how many went to each revision.
func (p *refPool) revisions(n int) map[string]int {
	got := map[string]int{}
	for i := 0; i < n; i++ {
		d, _ := p.route(shortPrompt, false)
		got[revisionOf(p.t, d)]++
	}
	return got
}

// TestScreenerFollowsThePodsTheEmbeddingCreates: the screener learns pods from the notifications
// SetEndpoint delivers. With revision "a" complete (2 prefill, 2 decode) and "b" lacking a decode
// pod, every request goes to "a" and pairs a prefill and a decode pod of one revision; when "b"
// gets a decode pod the split follows max-role (decode pods 2:1); when "b" loses its last prefill
// pod, all traffic returns to "a". Without the feed the screener sees no pod and every request is
// rejected.
func TestScreenerFollowsThePodsTheEmbeddingCreates(t *testing.T) {
	p := newScreenerPool(t, 1)
	for n := 1; n <= 2; n++ {
		setPod(t, p.r, 1, n, true)
		setPod(t, p.r, 2, n, true)
	}
	setPod(t, p.r, 4, 1, true) // revision b: a prefill pod and no decode pod
	p.settle()
	if got := p.revisions(60); got["a"] != 60 {
		t.Fatalf("revision b has no decode pod, but %v", got)
	}

	setPod(t, p.r, 5, 1, true) // revision b is now complete: decode pods a:2, b:1
	p.settle()
	got := p.revisions(300)
	if got["b"] < 75 || got["b"] > 125 {
		t.Errorf("revision b should take about a third of 300 requests (max-role weights 2:1), got %v", got)
	}

	setPod(t, p.r, 5, 1, false) // b's only decode pod stops being Ready
	p.settle()
	if got := p.revisions(60); got["a"] != 60 {
		t.Errorf("revision b has no Ready decode pod, but %v", got)
	}
	setPod(t, p.r, 5, 1, true)
	p.settle()
	if got := p.revisions(300); got["b"] == 0 {
		t.Fatalf("revision b is complete again but took nothing: %v", got)
	}
	if err := p.r.RemoveEndpoint("10.0.4.1"); err != nil { // b's only prefill pod is deleted
		t.Fatal(err)
	}
	p.settle()
	if got := p.revisions(60); got["a"] != 60 {
		t.Errorf("revision b has no prefill pod, but %v", got)
	}
}

// TestScreenerRevisionDrawIsTheRuntimesStream: the revision draw comes from the hosting
// runtime's seeded stream, so one seed gives one sequence of revisions and another seed another.
func TestScreenerRevisionDrawIsTheRuntimesStream(t *testing.T) {
	sequence := func(seed int64) string {
		p := newScreenerPool(t, seed)
		for _, rev := range []byte{1, 2, 4, 5} {
			setPod(t, p.r, rev, 1, true)
		}
		p.settle()
		var sb strings.Builder
		for i := 0; i < 64; i++ {
			d, _ := p.route(shortPrompt, false)
			sb.WriteString(revisionOf(t, d))
		}
		return sb.String()
	}
	a, again, other := sequence(1), sequence(1), sequence(2)
	if a != again {
		t.Errorf("the same seed gave different revision sequences:\n %s\n %s", a, again)
	}
	if a == other {
		t.Errorf("seeds 1 and 2 gave the same revision sequence %s", a)
	}
	if !strings.Contains(a, "a") || !strings.Contains(a, "b") {
		t.Errorf("two equal revisions should both be drawn in 64 requests: %s", a)
	}
}

// TestScreenerPodsMustCarryTheirLabels: a pod the pool selects without the revision label (or in
// scope without the role label) would be dropped from every request, so SetEndpoint refuses it
// by name; a pod outside the pool, or any pod when gating is off, is not checked.
func TestScreenerPodsMustCarryTheirLabels(t *testing.T) {
	p := newScreenerPool(t, 1)
	err := trySetPod(p.r, 1, 1, true, false, true)
	var re *contract.RefusalError
	if !errors.As(err, &re) || re.Type != "disaggregatedset-rollout-screener" ||
		!strings.Contains(re.Reason, "pod 10.0.1.1") || !strings.Contains(re.Reason, "disaggregatedset.x-k8s.io/revision") ||
		!strings.HasPrefix(err.Error(), refusedPrefix) {
		t.Errorf("a pod without the revision label: got %v", err)
	}
	err = trySetPod(p.r, 1, 2, true, true, false)
	if !errors.As(err, &re) || !strings.Contains(re.Reason, "disaggregatedset.x-k8s.io/role") {
		t.Errorf("a pod in scope without the role label: got %v", err)
	}
	outside := contract.Endpoint{Name: "x", IP: "10.9.9.9", Port: 8000, Labels: map[string]string{"app": "other"}}
	if err := p.r.SetEndpoint(outside, true); err != nil {
		t.Errorf("a pod the pool does not select is refused: %v", err)
	}
	if err := trySetPod(p.r, 1, 3, true, true, true); err != nil {
		t.Errorf("a fully labelled pod is refused: %v", err)
	}

	off := strings.Replace(screenerConfig, "mode: max-role", "mode: disabled", 1)
	r, err := New(context.Background(), embedtest.NewRuntime(1), contract.Config{Flags: experimentalFlags, Config: []byte(off)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Stop)
	if err := trySetPod(r, 1, 1, true, false, false); err != nil {
		t.Errorf("with revision gating off a pod without labels is refused: %v", err)
	}
}

// TestP2PSourceProducerIsRefusedByName: the producer sets a header that asks a peer engine to
// pull KV blocks. Without a P2P KV tier in the engines nothing would pull, so it stays refused,
// with a reason that names the tier, and the host's policy cannot lift it.
func TestP2PSourceProducerIsRefusedByName(t *testing.T) {
	cfg := withConfig("plugins:\n- type: p2p-source-producer\n- type: queue-scorer\n- type: max-score-picker\n- type: single-profile-handler\n" +
		"schedulingProfiles:\n- name: default\n  plugins:\n  - pluginRef: queue-scorer\n  - pluginRef: max-score-picker\n")
	cfg.Flags = experimentalFlags
	cfg.Allow = func(string) (bool, string) { return true, "" }
	_, err := build(t, cfg)
	var re *contract.RefusalError
	if !errors.As(err, &re) || re.Type != "p2p-source-producer" || re.Reason == notAudited || !strings.Contains(re.Reason, "P2P KV tier") {
		t.Fatalf("want p2p-source-producer refused with a reason naming the P2P KV tier, got %v", err)
	}
}
