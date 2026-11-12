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
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/inference-sim/blis-substrate/contracts/conformance/embedtest"
	contract "github.com/inference-sim/blis-substrate/contracts/embed"
	"sigs.k8s.io/yaml"
)

// The index (reference-configs/policies.json) lists the parameters of each policy a search may
// sweep (knobs) and the ones the router accepts that cannot change a hosted decision
// (ineffective_parameters). These tests hold both lists to the hosted router: a knob moves a
// decision, an ineffective parameter does not.

type indexKnob struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Plugin    string `json:"plugin"`
	Parameter string `json:"parameter"`
	Profile   string `json:"profile"`
	AppliesTo string `json:"applies_to"`
}

type indexPolicy struct {
	Name  string `json:"name"`
	Files struct {
		Colocated *string `json:"colocated"`
		PD        *string `json:"pd"`
	} `json:"files"`
	Knobs                 []indexKnob `json:"knobs"`
	IneffectiveParameters []indexKnob `json:"ineffective_parameters"`
}

func readPolicies(t *testing.T) []indexPolicy {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "reference-configs", "policies.json"))
	if err != nil {
		t.Fatal(err)
	}
	var idx struct {
		Policies []indexPolicy `json:"policies"`
	}
	if err := json.Unmarshal(b, &idx); err != nil {
		t.Fatal(err)
	}
	return idx.Policies
}

// shapes returns the files of a policy that carry a knob, by shape name.
func (p indexPolicy) shapes(k indexKnob) map[string]string {
	out := map[string]string{}
	if p.Files.Colocated != nil && k.AppliesTo != "pd" {
		out["colocated"] = *p.Files.Colocated
	}
	if p.Files.PD != nil && k.AppliesTo != "colocated" {
		out["pd"] = *p.Files.PD
	}
	return out
}

// edit applies fn to the configuration document and returns the new text.
func edit(t *testing.T, text string, fn func(doc map[string]any)) string {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		t.Fatal(err)
	}
	fn(doc)
	out, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// setParam sets a parameter of every plugin of the type, adding the plugin when the file leaves
// it to the router (the producers the router creates itself).
func setParam(doc map[string]any, typ, key string, value any) {
	plugins, _ := doc["plugins"].([]any)
	set := false
	for _, pl := range plugins {
		m := pl.(map[string]any)
		if m["type"] != typ {
			continue
		}
		params, _ := m["parameters"].(map[string]any)
		if params == nil {
			params = map[string]any{}
			m["parameters"] = params
		}
		params[key], set = value, true
	}
	if !set {
		doc["plugins"] = append(plugins, map[string]any{"type": typ, "parameters": map[string]any{key: value}})
	}
}

// setWeight sets the weight of the scorer in the named profile ("" for every profile naming it).
func setWeight(t *testing.T, doc map[string]any, scorer, profile string, w float64) {
	t.Helper()
	set := false
	for _, prof := range doc["schedulingProfiles"].([]any) {
		pm := prof.(map[string]any)
		if profile != "" && pm["name"] != profile {
			continue
		}
		for _, ref := range pm["plugins"].([]any) {
			rm := ref.(map[string]any)
			if rm["pluginRef"] == scorer {
				rm["weight"], set = w, true
			}
		}
	}
	if !set {
		t.Fatalf("no profile %q weights %s", profile, scorer)
	}
}

// knobState is what the stub engines report; tests change it between requests.
type knobState struct {
	waiting map[string]int
	kv      map[string]float64
	block   int // cache_config_info block_size when above 0
}

// kpool is a hosted router over stub engines whose metrics the test sets.
type kpool struct {
	*refPool
	st    *knobState
	roles []string            // "endpoint" for a colocated pool, else "prefill" and "decode"
	eps   map[string][]string // the endpoints of each role, "first" first
}

func newKnobPool(t *testing.T, cfg string, pd bool) *kpool {
	t.Helper()
	st := &knobState{waiting: map[string]int{}, kv: map[string]float64{}}
	rt := embedtest.NewRuntime(1)
	rt.Engines = func(r *http.Request) (*http.Response, error) {
		h := r.URL.Hostname()
		body := fmt.Sprintf("# TYPE vllm:num_requests_waiting gauge\nvllm:num_requests_waiting{model_name=\"m\"} %d\n"+
			"# TYPE vllm:kv_cache_usage_perc gauge\nvllm:kv_cache_usage_perc{model_name=\"m\"} %g\n", st.waiting[h], st.kv[h])
		if st.block > 0 {
			body += fmt.Sprintf("# TYPE vllm:cache_config_info gauge\nvllm:cache_config_info{block_size=\"%d\",num_gpu_blocks=\"100000\"} 1\n", st.block)
		}
		return embedtest.Respond(r, http.StatusOK, "text/plain", body), nil
	}
	r, err := New(context.Background(), rt, contract.Config{Flags: defaultFlags, Config: []byte(cfg)})
	if err != nil {
		t.Fatalf("%v\n%s", err, cfg)
	}
	t.Cleanup(r.Stop)
	p := &kpool{refPool: &refPool{t: t, rt: rt, r: r}, st: st, eps: map[string][]string{}}
	add := func(role, ip, label string) {
		labels := map[string]string{"llm-d.ai/inference-serving": "true"}
		if label != "" {
			labels["llm-d.ai/role"] = label
		}
		if err := r.SetEndpoint(contract.Endpoint{Name: ip, IP: ip, Port: 8000, Labels: labels}, true); err != nil {
			t.Fatal(err)
		}
		p.eps[role] = append(p.eps[role], ip)
	}
	if pd {
		p.roles = []string{"prefill", "decode"}
		add("prefill", "10.0.1.1", "prefill")
		add("prefill", "10.0.1.2", "prefill")
		add("decode", "10.0.2.1", "decode")
		add("decode", "10.0.2.2", "decode")
	} else {
		p.roles = []string{"endpoint"}
		add("endpoint", "10.0.3.1", "")
		add("endpoint", "10.0.3.2", "")
	}
	p.settle()
	return p
}

// settle lets the router scrape the engines again.
func (p *refPool) settle() {
	p.rt.Settle()
	p.rt.Advance(200 * time.Millisecond)
}

// pick is the endpoint of the decision for a role.
func pick(d contract.Decision, role string) string {
	if role == "prefill" {
		return strings.TrimSuffix(d.Prefill, ":8000")
	}
	return strings.TrimSuffix(d.Endpoint, ":8000")
}

// loadFirst puts n waiting requests on the first endpoint of every role, or the second when
// second is set; the other endpoint reports none.
func (p *kpool) loadFirst(n int, second bool) {
	for _, eps := range p.eps {
		p.st.waiting[eps[0]], p.st.waiting[eps[1]] = n, 0
		if second {
			p.st.waiting[eps[0]], p.st.waiting[eps[1]] = 0, n
		}
	}
	p.settle()
}

// chosenBy routes n completed requests of distinct prompts and returns, for each role of roles,
// the sorted endpoints it chose.
func (p *kpool) chosenBy(n int, roles []string) string {
	seen := map[string]map[string]bool{}
	for i := 0; i < n; i++ {
		d, _ := p.route(longPrompt(500+i, 300), false)
		for _, role := range roles {
			if seen[role] == nil {
				seen[role] = map[string]bool{}
			}
			seen[role][pick(d, role)] = true
		}
	}
	out := make([]string, 0, len(roles))
	for _, role := range roles {
		var eps []string
		for ep := range seen[role] { // sorted below
			eps = append(eps, ep)
		}
		sort.Strings(eps)
		out = append(out, role+"="+strings.Join(eps, "+"))
	}
	return strings.Join(out, " ")
}

// scenario drives one knob: it returns a description of the decisions it made.
type scenario func(t *testing.T, cfg string, pd bool, roles []string) string

// queueScenario: the first endpoint of every role reports a queue of 3 (above 5 the router's
// saturation detector would drop the endpoint whatever the scorers say).
func queueScenario(t *testing.T, cfg string, pd bool, roles []string) string {
	p := newKnobPool(t, cfg, pd)
	p.loadFirst(3, false)
	return p.chosenBy(40, orRoles(roles, p.roles))
}

// kvScenario: the first endpoint of every role reports a half-full KV cache (the router's
// saturation detector drops an endpoint above 0.8).
func kvScenario(t *testing.T, cfg string, pd bool, roles []string) string {
	p := newKnobPool(t, cfg, pd)
	for _, eps := range p.eps {
		p.st.kv[eps[0]] = 0.5
	}
	p.settle()
	return p.chosenBy(40, orRoles(roles, p.roles))
}

func orRoles(roles, all []string) []string {
	if len(roles) == 0 {
		return all
	}
	return roles
}

// followScenario: 12 prompts, each sent twice (the second time with a fresh tail in a P/D pool,
// so that it is disaggregated again); counts how often the second request takes the first one's
// endpoint of the role.
func followScenario(t *testing.T, cfg string, pd bool, roles []string) string {
	p := newKnobPool(t, cfg, pd)
	role := orRoles(roles, p.roles)[len(orRoles(roles, p.roles))-1]
	same := 0
	for i := 0; i < 12; i++ {
		a := longPrompt(i, 300)
		first, _ := p.route(a, false)
		second := a
		if role == "prefill" {
			second = append(slices.Clone(a), longPrompt(100+i, 200)...)
		}
		again, _ := p.route(second, false)
		if pick(first, role) == pick(again, role) {
			same++
		}
	}
	return fmt.Sprintf("%s follows %d of 12", role, same)
}

// partialScenario: 12 prompts, each followed by one that shares only its first 128 tokens; counts
// how often the second takes the endpoint of the first.
func partialScenario(st func(*knobState)) scenario {
	return func(t *testing.T, cfg string, pd bool, roles []string) string {
		p := newKnobPool(t, cfg, pd)
		if st != nil {
			st(p.st)
			p.settle()
		}
		same := 0
		for i := 0; i < 12; i++ {
			a := longPrompt(i, 300)
			b := append(slices.Clone(a[:128]), longPrompt(100+i, 172)...)
			first, _ := p.route(a, false)
			second, _ := p.route(b, false)
			if pick(first, "decode") == pick(second, "decode") && pick(first, "endpoint") == pick(second, "endpoint") {
				same++
			}
		}
		return fmt.Sprintf("%d of 12 follow", same)
	}
}

// holderScenario: the first endpoints cache a 256-token prompt while the second endpoints are
// loaded; then the first endpoints get a queue of 1 and the second ones none, and a query goes in.
// The query is the decision under test: which endpoints it takes depends on how the prefix score
// of the cached endpoint weighs against the queue score of the idle one.
func holderScenario(query func(seed []int) []int) scenario {
	return func(t *testing.T, cfg string, pd bool, roles []string) string {
		p := newKnobPool(t, cfg, pd)
		seed := longPrompt(1, 256)
		p.loadFirst(100, true)
		p.route(seed, false)
		p.loadFirst(1, false)
		d, _ := p.route(query(seed), false)
		return fmt.Sprintf("endpoint %s prefill %s", d.Endpoint, d.Prefill)
	}
}

var (
	queryRepeat   = func(seed []int) []int { return seed }                                                   // 4 blocks, all cached
	queryOneBlock = func(seed []int) []int { return append(slices.Clone(seed[:64]), longPrompt(9, 236)...) } // 1 of 4 blocks cached
	queryLong     = func(seed []int) []int { return append(slices.Clone(seed), longPrompt(8, 7936)...) }     // 4 of 128 blocks cached
)

// heldScenario: one request is held in flight, then 20 completed ones follow; counts how many
// took the held request's endpoint in each role.
func heldScenario(prompt []int) scenario {
	return func(t *testing.T, cfg string, pd bool, roles []string) string {
		p := newKnobPool(t, cfg, pd)
		held, _ := p.route(prompt, true)
		onHeld := map[string]int{}
		for i := 0; i < 20; i++ {
			d, _ := p.route(longPrompt(500+i, 40), false)
			for _, role := range p.roles {
				if pick(d, role) == pick(held, role) {
					onHeld[role]++
				}
			}
		}
		out := make([]string, 0, len(p.roles))
		for _, role := range p.roles {
			out = append(out, fmt.Sprintf("%s=%d", role, onHeld[role]))
		}
		return "on the held endpoint: " + strings.Join(out, " ")
	}
}

// busyScenario: three requests are held (two endpoints end up with 2 and 1), then 20 completed
// ones follow; counts how many took the endpoint holding 2, per role.
func busyScenario(t *testing.T, cfg string, pd bool, roles []string) string {
	p := newKnobPool(t, cfg, pd)
	count := map[string]map[string]int{}
	for i := 0; i < 3; i++ {
		d, _ := p.route(longPrompt(700+i, 40), true)
		for _, role := range p.roles {
			if count[role] == nil {
				count[role] = map[string]int{}
			}
			count[role][pick(d, role)]++
		}
	}
	heavier := map[string]string{}
	for role, c := range count {
		for ep, n := range c {
			if n == 2 {
				heavier[role] = ep
			}
		}
	}
	onHeavy := map[string]int{}
	for i := 0; i < 20; i++ {
		d, _ := p.route(longPrompt(500+i, 40), false)
		for _, role := range p.roles {
			if pick(d, role) == heavier[role] {
				onHeavy[role]++
			}
		}
	}
	out := make([]string, 0, len(p.roles))
	for _, role := range p.roles {
		out = append(out, fmt.Sprintf("%s=%d", role, onHeavy[role]))
	}
	return "on the busier endpoint: " + strings.Join(out, " ")
}

// disaggScenario: 6 new 600-token prompts; counts how many the decider sends to a prefill endpoint.
func disaggScenario(t *testing.T, cfg string, pd bool, roles []string) string {
	p := newKnobPool(t, cfg, pd)
	n := 0
	for i := 0; i < 6; i++ {
		if d, _ := p.route(longPrompt(i, 600), false); d.Prefill != "" {
			n++
		}
	}
	return fmt.Sprintf("%d of 6 disaggregated", n)
}

// variant is the two settings of a knob a test compares.
type variant struct {
	name string
	set  func(t *testing.T, doc map[string]any)
}

func param(typ, key string, value any) func(*testing.T, map[string]any) {
	return func(_ *testing.T, doc map[string]any) { setParam(doc, typ, key, value) }
}

func all(fns ...func(*testing.T, map[string]any)) func(*testing.T, map[string]any) {
	return func(t *testing.T, doc map[string]any) {
		for _, fn := range fns {
			fn(t, doc)
		}
	}
}

func weight(scorer, profile string, w float64) func(*testing.T, map[string]any) {
	return func(t *testing.T, doc map[string]any) { setWeight(t, doc, scorer, profile, w) }
}

// knobTest says how to move one knob and what to watch while it moves.
type knobTest struct {
	a, b  variant
	watch scenario
}

const (
	prefixScorer = "prefix-cache-scorer"
	approx       = "approx-prefix-cache-producer"
)

// knobTests is keyed by the index's knob id ("weight.<scorer>" for a weight in every profile).
var knobTests = map[string]knobTest{
	"weight.queue-scorer":                     {variant{"0", weight("queue-scorer", "", 0)}, variant{"10", weight("queue-scorer", "", 10)}, queueScenario},
	"weight.prefill.queue-scorer":             {variant{"0", weight("queue-scorer", "prefill", 0)}, variant{"10", weight("queue-scorer", "prefill", 10)}, queueScenario},
	"weight.decode.queue-scorer":              {variant{"0", weight("queue-scorer", "decode", 0)}, variant{"10", weight("queue-scorer", "decode", 10)}, queueScenario},
	"weight.kv-cache-utilization-scorer":      {variant{"0", weight("kv-cache-utilization-scorer", "", 0)}, variant{"10", weight("kv-cache-utilization-scorer", "", 10)}, kvScenario},
	"weight.prefix-cache-scorer":              {variant{"0", weight(prefixScorer, "", 0)}, variant{"10", weight(prefixScorer, "", 10)}, followScenario},
	"weight.prefill.prefix-cache-scorer":      {variant{"0", weight(prefixScorer, "prefill", 0)}, variant{"10", weight(prefixScorer, "prefill", 10)}, followScenario},
	"weight.decode.prefix-cache-scorer":       {variant{"0", weight(prefixScorer, "decode", 0)}, variant{"10", weight(prefixScorer, "decode", 10)}, followScenario},
	"weight.active-request-scorer":            {variant{"0", weight("active-request-scorer", "", 0)}, variant{"1", weight("active-request-scorer", "", 1)}, heldScenario(longPrompt(3, 40))},
	"active-request-scorer.idleThreshold":     {variant{"0", param("active-request-scorer", "idleThreshold", 0)}, variant{"1", param("active-request-scorer", "idleThreshold", 1)}, heldScenario(longPrompt(3, 40))},
	"active-request-scorer.maxBusyScore":      {variant{"1", param("active-request-scorer", "maxBusyScore", 1.0)}, variant{"0", param("active-request-scorer", "maxBusyScore", 0.0)}, busyScenario},
	"prefix-based-pd-decider.nonCachedTokens": {variant{"16", param("prefix-based-pd-decider", "nonCachedTokens", 16)}, variant{"100000", param("prefix-based-pd-decider", "nonCachedTokens", 100000)}, disaggScenario},
	"prefix-based-pd-decider.promptTokens":    {variant{"0", param("prefix-based-pd-decider", "promptTokens", 0)}, variant{"100000", param("prefix-based-pd-decider", "promptTokens", 100000)}, disaggScenario},
	"prefix-cache-scorer.matchLengthWeight": {variant{"0", param(prefixScorer, "matchLengthWeight", 0.0)},
		variant{"1 over 256 tokens", all(param(prefixScorer, "matchLengthWeight", 1.0), param(prefixScorer, "matchLengthScaleTokens", 256))}, holderScenario(queryLong)},
	"prefix-cache-scorer.matchLengthScaleTokens": {variant{"1000000", all(param(prefixScorer, "matchLengthWeight", 1.0), param(prefixScorer, "matchLengthScaleTokens", 1000000))},
		variant{"256", all(param(prefixScorer, "matchLengthWeight", 1.0), param(prefixScorer, "matchLengthScaleTokens", 256))}, holderScenario(queryLong)},
	"approx-prefix-cache-producer.maxPrefixTokensToMatch": {variant{"131072", param(approx, "maxPrefixTokensToMatch", 131072)}, variant{"64", param(approx, "maxPrefixTokensToMatch", 64)}, holderScenario(queryOneBlock)},
	"approx-prefix-cache-producer.lruCapacityPerServer":   {variant{"31250", param(approx, "lruCapacityPerServer", 31250)}, variant{"1", param(approx, "lruCapacityPerServer", 1)}, holderScenario(queryRepeat)},
	"approx-prefix-cache-producer.blockSizeTokens":        {variant{"64", param(approx, "blockSizeTokens", 64)}, variant{"256", param(approx, "blockSizeTokens", 256)}, partialScenario(nil)},
	"approx-prefix-cache-producer.autoTune": {variant{"false", all(param(approx, "autoTune", false), param(approx, "blockSizeTokens", 64))}, variant{"true", param(approx, "autoTune", true)},
		partialScenario(func(st *knobState) { st.block = 256 })},
}

// TestKnobMovesAHostedDecision: every knob the index lists changes what the hosted router
// decides when moved, in every file of its policy that carries it. The router runs over stub
// engines whose queue, KV usage and cache block size the scenario sets; the two settings of a
// knob must give different decisions in the scenario made for it.
func TestKnobMovesAHostedDecision(t *testing.T) {
	for _, pol := range readPolicies(t) {
		for _, k := range pol.Knobs {
			kt, ok := knobTests[k.ID]
			for shape, file := range pol.shapes(k) {
				t.Run(pol.Name+"/"+shape+"/"+k.ID, func(t *testing.T) {
					if !ok {
						t.Fatalf("the index lists %s as a knob and no hosted test moves it", k.ID)
					}
					text := refConfig(t, file)
					run := func(v variant) string {
						cfg := edit(t, text, func(doc map[string]any) { v.set(t, doc) })
						var roles []string
						if k.Profile != "" {
							roles = []string{k.Profile}
						}
						return kt.watch(t, cfg, shape == "pd", roles)
					}
					got := [2]string{run(kt.a), run(kt.b)}
					if got[0] == got[1] {
						t.Errorf("%s = %s and %s decide alike: %s", k.ID, kt.a.name, kt.b.name, got[0])
					}
					t.Logf("%s=%s: %s | %s=%s: %s", k.ID, kt.a.name, got[0], k.ID, kt.b.name, got[1])
				})
			}
		}
	}
}

// ineffectiveTest says how to set an ineffective parameter and which scenario would show an
// effect; the decisions with the parameter set must equal those without it.
type ineffectiveTest struct {
	set   func(t *testing.T, doc map[string]any)
	watch scenario
}

var ineffectiveTests = map[string]ineffectiveTest{
	"picker.maxNumOfEndpoints": {func(t *testing.T, doc map[string]any) {
		// The router adds a max-score-picker when the file has no picker: declare one.
		plugins, _ := doc["plugins"].([]any)
		have := false
		for _, pl := range plugins {
			switch pl.(map[string]any)["type"] {
			case "max-score-picker", "random-picker", "weighted-random-picker":
				have = true
				setParam(doc, pl.(map[string]any)["type"].(string), "maxNumOfEndpoints", 3)
			}
		}
		if !have {
			setParam(doc, "max-score-picker", "maxNumOfEndpoints", 3)
			for _, prof := range doc["schedulingProfiles"].([]any) {
				pm := prof.(map[string]any)
				pm["plugins"] = append(pm["plugins"].([]any), map[string]any{"pluginRef": "max-score-picker"})
			}
		}
	}, queueScenario},
	"inflight-load-producer.addEstimatedOutputTokens":     {param("inflight-load-producer", "addEstimatedOutputTokens", true), heldScenario(longPrompt(3, 4000))},
	"prefix-based-pd-decider.prefixMatchInfoProducerName": {param("prefix-based-pd-decider", "prefixMatchInfoProducerName", approx), followScenario},
	"approx-prefix-cache-producer.maxPrefixBlocksToMatch": {param(approx, "maxPrefixBlocksToMatch", 1), holderScenario(queryOneBlock)},
}

// TestIneffectiveParametersLeaveTheDecisionAlone: a parameter the index lists as ineffective can
// be set, and the router still decides as it did without it, in the scenario in which an
// effect would show. The router returns one endpoint whatever maxNumOfEndpoints says.
func TestIneffectiveParametersLeaveTheDecisionAlone(t *testing.T) {
	for _, pol := range readPolicies(t) {
		for _, k := range pol.IneffectiveParameters {
			it, ok := ineffectiveTests[k.ID]
			for shape, file := range pol.shapes(k) {
				t.Run(pol.Name+"/"+shape+"/"+k.ID, func(t *testing.T) {
					if !ok {
						t.Fatalf("the index lists %s as ineffective and no hosted test shows it", k.ID)
					}
					text := refConfig(t, file)
					without := it.watch(t, text, shape == "pd", nil)
					with := it.watch(t, edit(t, text, func(doc map[string]any) { it.set(t, doc) }), shape == "pd", nil)
					if with != without {
						t.Errorf("%s changed the decisions:\n without: %s\n with:    %s", k.ID, without, with)
					}
				})
			}
		}
	}
}

// TestPrefixMatchProducerNameSelectsOnly: any producer name but the one the file declares fails
// to load, so the parameter has no second value to sweep.
func TestPrefixMatchProducerNameSelectsOnly(t *testing.T) {
	cfg := edit(t, refConfig(t, "pd-prefix-based.yaml"), func(doc map[string]any) {
		setParam(doc, "prefix-based-pd-decider", "prefixMatchInfoProducerName", "no-such-producer")
	})
	if _, err := build(t, contract.Config{Flags: defaultFlags, Config: []byte(cfg)}); err == nil {
		t.Fatal("a decider naming a producer the file does not declare loads")
	}
}

// TestMaxNumOfEndpointsAboveOneStillGivesOneEndpoint: the decision carries one endpoint.
func TestMaxNumOfEndpointsAboveOneStillGivesOneEndpoint(t *testing.T) {
	cfg := edit(t, refConfig(t, "round-robin.yaml"), func(doc map[string]any) { setParam(doc, "max-score-picker", "maxNumOfEndpoints", 3) })
	p := newKnobPool(t, cfg, false)
	for i := 0; i < 10; i++ {
		d, _ := p.route(longPrompt(i, 40), false)
		if strings.Contains(d.Endpoint, ",") {
			t.Fatalf("the decision names several endpoints: %q", d.Endpoint)
		}
	}
}
