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
	"errors"
	"strings"
	"testing"

	contract "github.com/inference-sim/blis-substrate/contracts/embed"
)

// guideEntries are the indexed llm-d guide files (reference-configs/guides).
func guideEntries(t *testing.T) []indexPolicy {
	t.Helper()
	var out []indexPolicy
	for _, p := range readPolicies(t) {
		for _, f := range []*string{p.Files.Colocated, p.Files.PD} {
			if f != nil && strings.HasPrefix(*f, "guides/") {
				out = append(out, p)
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("the index holds no guide entry")
	}
	return out
}

func (p indexPolicy) file() (name string, pd bool) {
	if p.Files.PD != nil {
		return *p.Files.PD, true
	}
	return *p.Files.Colocated, false
}

// TestGuideConfigsRoute: each indexed guide file, given to the hosted router on the command line
// and pods its requires block states, routes requests: a P/D file names a prefill endpoint of the
// prefill pods and a decode endpoint of the decode pods for every request, a colocated file an
// endpoint of the pool.
func TestGuideConfigsRoute(t *testing.T) {
	for _, pol := range guideEntries(t) {
		name, pd := pol.file()
		t.Run(pol.Name, func(t *testing.T) {
			p := newKnobPool(t, refConfig(t, name), env{pd: pd, spec: pol.Requires})
			for i := 0; i < 12; i++ {
				d, _ := p.route(longPrompt(i, 300), false)
				if pd {
					if !strings.HasPrefix(d.Prefill, "10.0.1.") || !strings.HasPrefix(d.Endpoint, "10.0.2.") {
						t.Fatalf("request %d: prefill %q decode %q, want a prefill pod and a decode pod", i, d.Prefill, d.Endpoint)
					}
				} else if !strings.HasPrefix(d.Endpoint, "10.0.3.") || d.Prefill != "" {
					t.Fatalf("request %d: endpoint %q prefill %q, want one endpoint of the pool", i, d.Endpoint, d.Prefill)
				}
			}
		})
	}
}

// TestGuideConfigsKeepACachedPrefix: the files that gate on the prefix-cache-affinity-filter
// keep a query on the endpoint that holds its prefix when that endpoint is not much slower, and
// the files that score the prefix follow it.
func TestGuideConfigsKeepACachedPrefix(t *testing.T) {
	for _, pol := range guideEntries(t) {
		name, pd := pol.file()
		text := refConfig(t, name)
		t.Run(pol.Name, func(t *testing.T) {
			switch {
			case strings.Contains(text, "prefix-cache-affinity-filter"):
				got := affinityScenario(strings.Contains(text, "precise-prefix-cache-producer"), nil)(t, text, env{pd: pd, spec: pol.Requires})
				if got != "the query stays on the cached endpoint" {
					t.Errorf("%s", got)
				}
			case strings.Contains(text, "prefix-cache-scorer"):
				role := []string(nil)
				if pd {
					role = []string{"prefill"}
				}
				if got := followScenario(t, text, env{pd: pd, spec: pol.Requires, roles: role}); !strings.HasSuffix(got, "follows 12 of 12") {
					t.Errorf("%s", got)
				}
			default:
				t.Skip("no prefix-aware plugin")
			}
		})
	}
}

// TestGuideScreenerEntriesNeedTheirLabels: a guide file that gates on revisions refuses, when the
// pod is added, a pod of the pool that lacks the revision label its requires block lists, naming
// the label; with the labels every pod is accepted.
func TestGuideScreenerEntriesNeedTheirLabels(t *testing.T) {
	n := 0
	for _, pol := range guideEntries(t) {
		name, _ := pol.file()
		if !strings.Contains(refConfig(t, name), "disaggregatedset-rollout-screener") {
			continue
		}
		n++
		t.Run(pol.Name, func(t *testing.T) {
			_, pd := pol.file()
			p := newKnobPool(t, refConfig(t, name), env{pd: pd, spec: pol.Requires}) // accepts every pod with the labels
			labels := map[string]string{"llm-d.ai/inference-serving": "true", "llm-d.ai/role": "prefill"}
			for k, v := range pol.Requires.Labels["all"] {
				if k != "disaggregatedset.x-k8s.io/revision" {
					labels[k] = v
				}
			}
			err := p.r.SetEndpoint(contract.Endpoint{Name: "bare", IP: "10.0.9.9", Port: 8000, Labels: labels}, true)
			var re *contract.RefusalError
			if !errors.As(err, &re) || !strings.Contains(re.Reason, "disaggregatedset.x-k8s.io/revision") {
				t.Errorf("a pod without the revision label: got %v", err)
			}
		})
	}
	if n == 0 {
		t.Fatal("no guide entry gates on revisions")
	}
}
