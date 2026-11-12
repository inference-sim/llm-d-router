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

package referenceconfigs

import (
	"encoding/json"
	"io/fs"
	"strings"
	"testing"
)

// namedFiles returns every string in the decoded index that names a YAML file.
func namedFiles(v any, out map[string]bool) {
	switch x := v.(type) {
	case string:
		if strings.HasSuffix(x, ".yaml") && !strings.ContainsAny(x, " ,") {
			out[x] = true
		}
	case []any:
		for _, e := range x {
			namedFiles(e, out)
		}
	case map[string]any:
		for _, e := range x {
			namedFiles(e, out)
		}
	}
}

func TestEveryFileNamedInPoliciesIsInFS(t *testing.T) {
	raw, err := fs.ReadFile(FS, "policies.json")
	if err != nil {
		t.Fatalf("policies.json is not in FS: %v", err)
	}
	var index any
	if err := json.Unmarshal(raw, &index); err != nil {
		t.Fatalf("policies.json does not parse: %v", err)
	}
	names := map[string]bool{}
	namedFiles(index, names)
	if len(names) == 0 {
		t.Fatal("policies.json names no file; the walk is broken")
	}
	for name := range names {
		if _, err := fs.Stat(FS, name); err != nil {
			t.Errorf("policies.json names %q but FS lacks it: %v", name, err)
		}
	}
}

func TestEveryEmbeddedConfigIsNamedInPolicies(t *testing.T) {
	raw, err := fs.ReadFile(FS, "policies.json")
	if err != nil {
		t.Fatal(err)
	}
	var index any
	if err := json.Unmarshal(raw, &index); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	namedFiles(index, names)
	err = fs.WalkDir(FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(p, ".yaml") && !names[p] {
			t.Errorf("FS carries %q but policies.json does not name it", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
