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
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

// The X20 deferral (release plan, "Deferring signing and sign-off"). blis-substrate's
// .github/x20-deferral.yaml, read from the substrate version go.mod pins, lets the commit policy
// report two findings as deferred for a stated time: a commit is not signed, and it has no
// Signed-off-by line. Every other finding still fails. A missing file defers nothing. A
// malformed, future-dated, too-long or expired file defers nothing and is itself a finding.
//
// This mirrors tools/prcheck/deferral.go in blis-substrate, which the shared workflows run.

const deferralPath = ".github/x20-deferral.yaml"

// maxDeferral is the longest a deferral may last, counted from the day it was added.
const maxDeferral = 30 * 24 * time.Hour

var issueName = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+#[0-9]+$`)

type deferral struct {
	Issue   string `json:"issue"`
	Added   string `json:"added"`
	Expires string `json:"expires"`
	Reason  string `json:"reason"`
}

// readDeferral returns the deferral file of the blis-substrate version the module in dir
// requires, or nil when that version has none.
func readDeferral(dir string) ([]byte, error) {
	modDir, err := substrateDir(dir)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(modDir, filepath.FromSlash(deferralPath)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

// deferrable reports whether a finding is one of the two the deferral covers. Findings read
// "<sha> <subject>: <reason>", so the reason is matched at the end of the line.
func deferrable(finding string) bool {
	return strings.HasSuffix(finding, ": it is not signed with a verified signature (X20)") ||
		strings.HasSuffix(finding, ": it has no Signed-off-by line from the person who takes responsibility for it (X20)")
}

// applyDeferral splits findings into those that fail and those the deferral file src covers on
// the given day.
func applyDeferral(src []byte, today time.Time, findings []string) (fail, deferred []string) {
	if len(bytes.TrimSpace(src)) == 0 {
		return findings, nil
	}
	today = time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)
	var d deferral
	if err := yaml.UnmarshalStrict(src, &d); err != nil {
		return append(append([]string{}, findings...), fmt.Sprintf("x20-deferral.yaml: %v", err)), nil
	}
	var bad []string
	if !issueName.MatchString(d.Issue) {
		bad = append(bad, fmt.Sprintf("x20-deferral.yaml: issue %q is not <owner>/<repo>#<n> (X20)", d.Issue))
	}
	if strings.TrimSpace(d.Reason) == "" {
		bad = append(bad, "x20-deferral.yaml: needs a reason")
	}
	added, aerr := time.Parse("2006-01-02", d.Added)
	if aerr != nil {
		bad = append(bad, fmt.Sprintf("x20-deferral.yaml: added %q is not a date YYYY-MM-DD", d.Added))
	} else if added.After(today) {
		bad = append(bad, fmt.Sprintf("x20-deferral.yaml: added %s is in the future", d.Added))
	}
	exp, err := time.Parse("2006-01-02", d.Expires)
	if err != nil {
		bad = append(bad, fmt.Sprintf("x20-deferral.yaml: expires %q is not a date YYYY-MM-DD", d.Expires))
	} else {
		if aerr == nil && exp.Sub(added) > maxDeferral {
			bad = append(bad, fmt.Sprintf("x20-deferral.yaml: expires %s is more than 30 days after added %s", d.Expires, d.Added))
		}
		if !today.Before(exp) {
			bad = append(bad, fmt.Sprintf("x20-deferral.yaml: expired on %s; sign and sign off the commits, or renew it with a reason (X20)", d.Expires))
		}
	}
	if len(bad) > 0 {
		return append(append([]string{}, findings...), bad...), nil
	}
	for _, f := range findings {
		if deferrable(f) {
			deferred = append(deferred, f)
		} else {
			fail = append(fail, f)
		}
	}
	return fail, deferred
}
