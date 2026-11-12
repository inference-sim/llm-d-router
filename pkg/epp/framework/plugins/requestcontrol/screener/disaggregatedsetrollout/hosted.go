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

import (
	"fmt"

	"k8s.io/apimachinery/pkg/labels"
)

// CheckHostedPod reports why a pod the router would route to, with the given labels, would
// vanish from every revision-gated request, or nil when the screener can see it.
//
// With revision gating on, the screener keeps an endpoint only when its revision label names a
// revision that has a Ready pod in every required role, and learns the roles and revisions from
// the pods in its scope that carry both labels. A pod without a revision label is therefore
// never kept, and a pod in scope without a role label is never counted. A deployment's
// controller sets the labels; an embedding states them, and a missing one leaves the screener
// with no endpoint for any request. The embedding calls this when it adds or changes a pod so
// the mistake is reported at the pod, not as a run in which nothing is routed.
func (c *Screener) CheckHostedPod(podLabels map[string]string) error {
	if !c.config.RevisionGating.Active() {
		return nil
	}
	if podLabels[c.revisionLabelKey] == "" {
		return fmt.Errorf("it lacks the revision label %q, so revision gating would drop it from every request", c.revisionLabelKey)
	}
	if c.scope.Matches(labels.Set(podLabels)) && podLabels[c.roleLabelKey] == "" {
		return fmt.Errorf("it is in scope %q but lacks the role label %q, so revision gating would not count it", c.config.Scope.LabelSelector, c.roleLabelKey)
	}
	return nil
}
