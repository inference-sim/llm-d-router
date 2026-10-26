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
	"go/constant"
	"go/types"
	"testing"

	contract "github.com/inference-sim/blis-schemas/embed"
	"golang.org/x/tools/go/packages"

	errcommon "github.com/llm-d/llm-d-router/pkg/common/error"
)

// TestClassify_AllowList: each router outcome maps to exactly one RejectKind, and anything else
// is Unclassified with the error's full text, never counted as shedding. Every drop reason the
// router declares (every constant of type RequestDroppedReason in pkg/common/error, by type
// checking) must appear here, so an upstream release that adds one fails until it is mapped.
func TestClassify_AllowList(t *testing.T) {
	dropped := func(code string, r errcommon.RequestDroppedReason) error {
		return errcommon.Error{Code: code, Msg: "m", Headers: map[string]string{errcommon.RequestDroppedReasonHeaderKey: string(r)}}
	}
	cases := []struct {
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
	}
	covered := map[string]bool{}
	for _, tc := range cases {
		var e errcommon.Error
		if errors.As(tc.err, &e) {
			if r, ok := e.Headers[errcommon.RequestDroppedReasonHeaderKey]; ok {
				covered[r] = true
			}
		}
	}
	for _, r := range declaredDropReasons(t) {
		if !covered[r] {
			t.Errorf("drop reason %q is declared in pkg/common/error but has no case here; map it in classify and add it", r)
		}
	}
	for _, tc := range cases {
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

// declaredDropReasons are the values of every constant of type RequestDroppedReason that
// pkg/common/error declares, found by type-checking the package, so every declaration form
// counts (an explicit or inherited type, a conversion, a reference to another constant).
func declaredDropReasons(t *testing.T) []string {
	t.Helper()
	pkgs, err := packages.Load(&packages.Config{Mode: packages.NeedTypes | packages.NeedName}, "github.com/llm-d/llm-d-router/pkg/common/error")
	if err != nil || len(pkgs) != 1 || len(pkgs[0].Errors) > 0 {
		t.Fatalf("loading pkg/common/error: %v %v", err, pkgs)
	}
	scope := pkgs[0].Types.Scope()
	reason := scope.Lookup("RequestDroppedReason")
	if reason == nil {
		t.Fatal("pkg/common/error declares no RequestDroppedReason")
	}
	var out []string
	for _, name := range scope.Names() {
		c, ok := scope.Lookup(name).(*types.Const)
		if !ok || !types.Identical(c.Type(), reason.Type()) {
			continue
		}
		out = append(out, constant.StringVal(c.Val()))
	}
	if len(out) == 0 {
		t.Fatal("found no RequestDroppedReason constants")
	}
	return out
}
