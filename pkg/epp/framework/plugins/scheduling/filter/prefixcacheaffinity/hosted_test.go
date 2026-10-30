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

package prefixcacheaffinity

import (
	"context"
	"math/rand/v2"
	"testing"

	"github.com/llm-d/llm-d-router/pkg/common/hosting"
	fwksched "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/scheduling"
)

// streamRuntime is a hosted runtime whose only behaviour is its named random streams: the same
// name returns the same stream, seeded by the runtime's seed, and it records the names drawn.
type streamRuntime struct {
	hosting.Runtime
	seed    uint64
	streams map[string]*rand.Rand
}

func (r *streamRuntime) Hosted() bool { return true }
func (r *streamRuntime) Rand(name string) hosting.Rand {
	if r.streams[name] == nil {
		r.streams[name] = rand.New(rand.NewPCG(r.seed, 7)) //nolint:gosec // test
	}
	return r.streams[name]
}

// explorations runs n filter decisions under a hosted runtime of the given seed and records which
// skipped the gate (kept both endpoints).
func explorations(seed uint64, n int) ([]bool, *streamRuntime) {
	rt := &streamRuntime{Runtime: hosting.Real, seed: seed, streams: map[string]*rand.Rand{}}
	ctx := hosting.Into(context.Background(), rt)
	p := newTestPlugin(Config{AffinityThreshold: 0.80, ExplorationProbability: 0.5})
	out := make([]bool, n)
	for i := range out {
		eps := []fwksched.Endpoint{makeEndpoint("a", 90, 100, 0), makeEndpoint("b", 10, 50, 0)}
		out[i] = len(p.Filter(ctx, nil, eps)) == 2
	}
	return out, rt
}

// TestFilter_HostedExplorationIsTheRuntimes: under a hosted runtime the exploration draw comes
// from the runtime's own stream, named after the plugin type, so one seed reproduces the same
// decisions and another seed changes them; process-wide randomness is never read.
func TestFilter_HostedExplorationIsTheRuntimes(t *testing.T) {
	a, rt := explorations(1, 64)
	b, _ := explorations(1, 64)
	c, _ := explorations(2, 64)
	if len(rt.streams) != 1 || rt.streams[PluginType] == nil {
		t.Fatalf("drew from streams %v, want only %q", rt.streams, PluginType)
	}
	same, explored := true, 0
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("decision %d differs between two runtimes with the same seed", i)
		}
		same = same && a[i] == c[i]
		if a[i] {
			explored++
		}
	}
	if same {
		t.Error("two seeds made the same 64 exploration decisions: the draw does not come from the runtime")
	}
	if explored == 0 || explored == len(a) {
		t.Errorf("explored %d of %d at probability 0.5", explored, len(a))
	}
}
