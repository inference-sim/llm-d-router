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
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

// blisembed is the hosting contract, which lives in this module.
const blisembed = "github.com/llm-d/llm-d-router/blisembed"

// TestImportsNoInferenceSimPackage: the fork imports no github.com/inference-sim/ package, in any
// package or test, so it depends on no BLIS module and inference-sim can import it without a
// cycle (BLIS design X7, RTR-3). The hosting contract is its own blisembed package.
func TestImportsNoInferenceSimPackage(t *testing.T) {
	pkgs, err := packages.Load(&packages.Config{Mode: packages.NeedName | packages.NeedImports | packages.NeedDeps, Tests: true},
		"github.com/llm-d/llm-d-router/...")
	if err != nil {
		t.Fatal(err)
	}
	contract := 0
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		for _, e := range p.Errors {
			t.Errorf("%s: %v", p.PkgPath, e)
		}
		path := p.PkgPath
		switch {
		case path == blisembed:
			contract++
		case strings.HasPrefix(path, "github.com/inference-sim/"):
			t.Errorf("the build reaches %s: the fork imports no package of github.com/inference-sim/", path)
		}
	})
	if contract == 0 {
		t.Fatalf("%s was not reached: the test checked nothing", blisembed)
	}
}
