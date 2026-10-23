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
	"bytes"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var updateCRDs = flag.Bool("update-crds", false, "copy the router CRDs from their sources into crds/")

// crdSources are the crds/ copies' sources: this module's config/crd/bases ("" module), or a
// dependency's.
var crdSources = map[string]string{
	"llm-d.ai_inferenceobjectives.yaml":               "",
	"llm-d.ai_inferencemodelrewrites.yaml":            "",
	"inference.networking.k8s.io_inferencepools.yaml": "sigs.k8s.io/gateway-api-inference-extension",
}

// TestCRDsMatchModules: each embedded CRD is byte-identical to its source, so a release that
// changes a CRD's defaults changes what the embedded router reads. Regenerate with
// go test ./pkg/embed -run TestCRDsMatchModules -update-crds.
func TestCRDsMatchModules(t *testing.T) {
	for name, mod := range crdSources {
		dir := filepath.Join("..", "..")
		if mod != "" {
			out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", mod).Output()
			if err != nil {
				t.Fatalf("go list -m %s: %v", mod, err)
			}
			dir = strings.TrimSpace(string(out))
		}
		want, err := os.ReadFile(filepath.Join(dir, "config", "crd", "bases", name))
		if err != nil {
			t.Fatal(err)
		}
		if *updateCRDs {
			if err := os.WriteFile(filepath.Join("crds", name), want, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		got, err := crdFiles.ReadFile("crds/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("crds/%s differs from its source; rerun with -update-crds", name)
		}
	}
	entries, _ := crdFiles.ReadDir("crds")
	if len(entries) != len(crdSources) {
		t.Errorf("crds/ holds %d files, crdSources names %d", len(entries), len(crdSources))
	}
}

// TestCRDDefaulter_AppliesAPIServerDefaults: an InferenceObjective poolRef without a group gets
// the CRD's default group, as admission to an API server gives it; a version no CRD serves is
// refused.
func TestCRDDefaulter_AppliesAPIServerDefaults(t *testing.T) {
	res, err := parseManifests(newScheme(), []byte(testPool), defaultNamespace)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.objectives) != 1 || len(res.objectives[0].Spec.PoolRefs) != 1 || res.objectives[0].Spec.PoolRefs[0].Group == "" {
		t.Fatalf("want the objective's poolRef defaulted, got %+v", res.objectives)
	}
	_, err = parseManifests(newScheme(), []byte("apiVersion: llm-d.ai/v9\nkind: InferenceObjective\nmetadata:\n  name: x\n"), defaultNamespace)
	if err == nil || !strings.Contains(err.Error(), "is not served by the router's CRDs") {
		t.Fatalf("an unserved version: %v", err)
	}
}
