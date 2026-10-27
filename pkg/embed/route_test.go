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
	"maps"
	"testing"
	"time"

	contract "github.com/inference-sim/blis-schemas/embed"

	"github.com/llm-d/llm-d-router/pkg/epp/metadata"
)

// TestRequestHeaderMap: the headers a request carries are exactly the base set plus one per
// set field, rendered as BLIS renders them today: the TTFT objective in whole milliseconds,
// rounded up, and the TTL as time.Duration.String, each only when positive.
func TestRequestHeaderMap(t *testing.T) {
	base := map[string]string{":method": "POST", ":path": "/v1/completions", "content-type": "application/json", "x-request-id": "r1"}
	with := func(extra map[string]string) map[string]string {
		out := maps.Clone(base)
		maps.Copy(out, extra)
		return out
	}
	req := contract.Request{ID: "r1", Path: "/v1/completions"}
	for _, tc := range []struct {
		name string
		mod  func(*contract.Request)
		want map[string]string
	}{
		{"base only", func(*contract.Request) {}, base},
		{"objective and fairness", func(r *contract.Request) { r.Objective, r.FairnessID = "o", "f" },
			with(map[string]string{metadata.ObjectiveKey: "o", metadata.FlowFairnessIDKey: "f"})},
		{"TTFT 1us rounds up to 1ms", func(r *contract.Request) { r.TTFTSLO = time.Microsecond }, with(map[string]string{metadata.TTFTSLOHeaderKey: "1"})},
		{"TTFT 1000us is 1ms", func(r *contract.Request) { r.TTFTSLO = 1000 * time.Microsecond }, with(map[string]string{metadata.TTFTSLOHeaderKey: "1"})},
		{"TTFT 1001us rounds up to 2ms", func(r *contract.Request) { r.TTFTSLO = 1001 * time.Microsecond }, with(map[string]string{metadata.TTFTSLOHeaderKey: "2"})},
		{"TTFT 1ns rounds up to 1ms", func(r *contract.Request) { r.TTFTSLO = time.Nanosecond }, with(map[string]string{metadata.TTFTSLOHeaderKey: "1"})},
		{"TTFT zero is absent", func(r *contract.Request) { r.TTFTSLO = 0 }, base},
		{"TTFT negative is absent", func(r *contract.Request) { r.TTFTSLO = -time.Second }, base},
		{"TTL as Duration.String", func(r *contract.Request) { r.TTL = 1500 * time.Millisecond }, with(map[string]string{metadata.InferenceTTLHeaderKey: "1.5s"})},
		{"TTL microseconds", func(r *contract.Request) { r.TTL = 250 * time.Microsecond }, with(map[string]string{metadata.InferenceTTLHeaderKey: "250µs"})},
		{"TTL zero is absent", func(r *contract.Request) { r.TTL = 0 }, base},
		{"TTL negative is absent", func(r *contract.Request) { r.TTL = -time.Second }, base},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := req
			tc.mod(&r)
			if got := requestHeaderMap(r); !maps.Equal(got, tc.want) {
				t.Errorf("requestHeaderMap = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestHeaderMapIsSorted: headers reach the router in sorted key order, so the order the router
// sees never depends on map iteration (INV-6).
func TestHeaderMapIsSorted(t *testing.T) {
	h := headerMap(map[string]string{"x-b": "2", ":path": "/p", "x-a": "1", "content-type": "c"})
	keys := make([]string, 0, len(h.Headers))
	for _, v := range h.Headers {
		keys = append(keys, v.Key)
	}
	want := []string{":path", "content-type", "x-a", "x-b"}
	if len(keys) != len(want) {
		t.Fatalf("keys %v", keys)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("keys %v, want %v", keys, want)
		}
	}
}
