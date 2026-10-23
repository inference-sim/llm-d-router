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
	"testing"

	contract "github.com/inference-sim/blis-schemas/embed"

	errcommon "github.com/llm-d/llm-d-router/pkg/common/error"
)

// TestClassify_AllowList: each router outcome maps to exactly one RejectKind, and anything else
// is Unclassified with the error's full text, never counted as shedding. Every drop reason the
// router defines appears here, so an upstream release that adds one fails until it is mapped.
func TestClassify_AllowList(t *testing.T) {
	dropped := func(code string, r errcommon.RequestDroppedReason) error {
		return errcommon.Error{Code: code, Msg: "m", Headers: map[string]string{errcommon.RequestDroppedReasonHeaderKey: string(r)}}
	}
	for _, tc := range []struct {
		name string
		err  error
		want contract.RejectKind
	}{
		{"no endpoints", dropped(errcommon.ServiceUnavailable, errcommon.RequestDroppedReasonNoEndpoints), contract.NoEndpoints},
		{"ttl expired", dropped(errcommon.ResourceExhausted, errcommon.RequestDroppedReasonTTLExpired), contract.TTLExpired},
		{"queues at capacity", dropped(errcommon.ResourceExhausted, errcommon.RequestDroppedReasonSaturated), contract.Saturated},
		{"client gave up", dropped(errcommon.ServiceUnavailable, errcommon.RequestDroppedReasonContextCancelled), contract.ContextCancelled},
		{"legacy shed", errcommon.Error{Code: errcommon.ResourceExhausted, Msg: "system saturated, sheddable request dropped"}, contract.Admission},
		{"admitter plugin", errcommon.Error{Code: errcommon.ResourceExhausted, Msg: "probabilistic-admitter: rejected"}, contract.Admission},
		{"unavailable", errcommon.Error{Code: errcommon.ServiceUnavailable, Msg: "m"}, contract.Unavailable},
		{"shutting down", dropped(errcommon.ServiceUnavailable, errcommon.RequestDroppedReasonShuttingDown), contract.Unclassified},
		{"internal flow control", dropped(errcommon.Internal, errcommon.RequestDroppedReasonInternal), contract.Unclassified},
		{"evicted", dropped(errcommon.ServiceUnavailable, errcommon.RequestDroppedReasonEvicted), contract.Unclassified},
		{"evicted queue pressure", dropped(errcommon.ServiceUnavailable, errcommon.RequestDroppedReasonEvictedQueuePressure), contract.Unclassified},
		{"evicted priority", dropped(errcommon.ServiceUnavailable, errcommon.RequestDroppedReasonEvictedPriority), contract.Unclassified},
		{"internal", errcommon.Error{Code: errcommon.Internal, Msg: "m"}, contract.Unclassified},
		{"bad request", errcommon.Error{Code: errcommon.BadRequest, Msg: "m"}, contract.Unclassified},
		{"not found", errcommon.Error{Code: errcommon.NotFound, Msg: "m"}, contract.Unclassified},
		{"untyped scheduler failure", errcommon.Error{Code: errcommon.ResourceExhausted, Msg: "failed to find target endpoint: x"}, contract.Unclassified},
		{"untyped error", errors.New("boom"), contract.Unclassified},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, msg := classify(tc.err)
			if got != tc.want {
				t.Errorf("classify = %s, want %s", got, tc.want)
			}
			if got == contract.Unclassified && msg != tc.err.Error() {
				t.Errorf("Unclassified message %q, want the full error text %q", msg, tc.err.Error())
			}
		})
	}
}
