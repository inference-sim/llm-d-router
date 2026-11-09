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

// substrate is the module of the BLIS contracts.
const substrate = "github.com/inference-sim/blis-substrate"

// TestImportsOnlySubstrateContracts: the fork imports from blis-substrate only packages under
// contracts/, and no other BLIS module, in any package or test (BLIS design X7). The cost model,
// the catalog, the registry and the data loader are inference-sim's to import.
func TestImportsOnlySubstrateContracts(t *testing.T) {
	pkgs, err := packages.Load(&packages.Config{Mode: packages.NeedName | packages.NeedImports | packages.NeedDeps, Tests: true},
		"github.com/llm-d/llm-d-router/...")
	if err != nil {
		t.Fatal(err)
	}
	contracts := 0
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		for _, e := range p.Errors {
			t.Errorf("%s: %v", p.PkgPath, e)
		}
		path := p.PkgPath
		switch {
		case strings.HasPrefix(path, substrate+"/contracts/"):
			contracts++
		case path == substrate || strings.HasPrefix(path, substrate+"/"):
			t.Errorf("the build reaches %s: the fork imports only blis-substrate's contracts/ packages", path)
		case strings.HasPrefix(path, "github.com/inference-sim/"):
			t.Errorf("the build reaches %s: the fork imports no BLIS module but blis-substrate's contracts/", path)
		}
	})
	if contracts == 0 {
		t.Fatal("no package under blis-substrate's contracts/ was found: the test checked nothing")
	}
}
