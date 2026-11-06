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

package main

import (
	"strings"
	"testing"
	"time"
)

const goodDeferral = `issue: example/repo#12
added: 2026-11-05
expires: 2026-12-05
reason: signing needs a person
`

func TestApplyDeferral(t *testing.T) {
	day := func(s string) time.Time {
		d, err := time.Parse("2006-01-02", s)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	unsigned := `abc "s": it is not signed with a verified signature (X20)`
	noSignoff := `abc "s": it has no Signed-off-by line from the person who takes responsibility for it (X20)`
	coauthor := `abc "s": Co-authored-by "Claude <noreply@anthropic.com>" names a tool: say so in one Assisted-by line instead (X20)`
	spoof := `abc "it is not signed with a verified signature (X20)": it has 2 Assisted-by lines; agent help is named in one (X20)`
	all := []string{unsigned, noSignoff, coauthor, spoof}

	for _, tc := range []struct {
		name         string
		src, today   string
		wantFail     int
		wantDeferred int
		failMentions string
	}{
		{"holds: only the two findings are deferred", goodDeferral, "2026-11-06", 2, 2, "Co-authored-by"},
		{"no file defers nothing", "", "2026-11-06", 4, 0, ""},
		{"expired", goodDeferral, "2026-12-05", 5, 0, "expired on 2026-12-05"},
		{"malformed yaml", "issue: [", "2026-11-06", 5, 0, "x20-deferral.yaml"},
		{"unknown field", goodDeferral + "extra: 1\n", "2026-11-06", 5, 0, "x20-deferral.yaml"},
		{"bad issue", strings.Replace(goodDeferral, "example/repo#12", "twelve", 1), "2026-11-06", 5, 0, "not <owner>/<repo>#<n>"},
		{"no reason", strings.Replace(goodDeferral, "reason: signing needs a person\n", "", 1), "2026-11-06", 5, 0, "needs a reason"},
		{"longer than 30 days", strings.Replace(goodDeferral, "2026-12-05", "2026-12-06", 1), "2026-11-06", 5, 0, "more than 30 days"},
		{"added in the future", goodDeferral, "2026-11-04", 5, 0, "in the future"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fail, deferred := applyDeferral([]byte(tc.src), day(tc.today), all)
			if len(fail) != tc.wantFail || len(deferred) != tc.wantDeferred {
				t.Fatalf("fail %d %q, deferred %d %q; want %d and %d", len(fail), fail, len(deferred), deferred, tc.wantFail, tc.wantDeferred)
			}
			if tc.failMentions != "" && !strings.Contains(strings.Join(fail, "\n"), tc.failMentions) {
				t.Errorf("failures %q do not mention %q", fail, tc.failMentions)
			}
		})
	}
}

// TestReadDeferralFromPinnedSubstrate reads the file from the blis-substrate go.mod pins.
func TestReadDeferralFromPinnedSubstrate(t *testing.T) {
	b, err := readDeferral("../..")
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > 0 && !strings.Contains(string(b), "expires:") {
		t.Errorf("the pinned deferral file has no expires: %q", b)
	}
}
