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

package disaggregatedsetrollout

// sortedTotal adds the shares of the given revisions in the order given.
//
// A floating-point sum depends on the order of its terms in the last bit, and that bit can move a
// draw that lies on a cumulative boundary. pickWeightedRevision sums in Go map order, as upstream
// does, and calls sortedTotal only when hosted, so that two hosted runs with the same seed decide
// alike. Not hosted, upstream's order is kept (RTR-2, iteration order).
func sortedTotal(shares map[string]float64, revisions []string) float64 {
	total := 0.0
	for _, revision := range revisions {
		total += shares[revision]
	}
	return total
}
