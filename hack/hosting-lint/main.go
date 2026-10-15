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


// Command hosting-lint keeps the router hostable on the runtime in pkg/common/hosting.
//
// Every clock read, timer, deadline, goroutine and process-wide random draw on a router path
// must go through hosting.Runtime, or a simulator that embeds the router loses determinism.
// The lint counts the direct uses per file and fails when a file has more than the baseline
// records: a rebase onto a new release that adds one must either convert it or, for a use that
// cannot affect a routing decision (a latency metric, a log rate limit), add it to the baseline
// on review. A line annotated "//blis:wallclock-ok" is not counted.
//
// It also enforces the patch budget: the lines the fork changes in files it shares with
// upstream, so the cost of re-applying the fork on each release stays bounded.
//
//	go run ./hack/hosting-lint            check against hack/hosting-lint/baseline.txt
//	go run ./hack/hosting-lint -update    rewrite the baseline
//	go run ./hack/hosting-lint -budget    check the patch budget against UPSTREAM_BASE
package main

import (
	"bufio"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

var roots = []string{"pkg/epp", "pkg/kvcache", "pkg/kvevents", "pkg/common", "cmd/epp/runner"}

// excluded are paths whose code never runs in an embedded router.
var excluded = []string{
	"pkg/common/hosting/",                // the seam itself
	"pkg/epp/server/",                    // ext_proc, health and metrics servers
	"pkg/epp/handlers/server.go",         // the gRPC stream loop; the embedder drives the handlers
	"pkg/epp/controller/",                // Kubernetes reconcilers run by a controller manager
	"pkg/epp/framework/plugins/datalayer/source/mocks/",
	"pkg/epp/framework/interface/flowcontrol/mocks/",
	"pkg/epp/datalayer/mocks/",
	"pkg/epp/framework/plugins/requestcontrol/dataproducer/predictedlatency/latencypredictorclient/tests/",
}

const baselinePath = "hack/hosting-lint/baseline.txt"
const budgetPath = "hack/hosting-lint/budget.txt"

func main() {
	update := flag.Bool("update", false, "rewrite the baseline")
	budget := flag.Bool("budget", false, "check the patch budget")
	flag.Parse()
	if *budget {
		os.Exit(checkBudget())
	}
	counts := scan()
	if *update {
		writeBaseline(counts)
		return
	}
	base := readBaseline()
	var failures []string
	for key, n := range counts {
		if n > base[key] {
			failures = append(failures, fmt.Sprintf("%s: %d direct use(s), baseline %d", key, n, base[key]))
		}
	}
	sort.Strings(failures)
	if len(failures) > 0 {
		fmt.Println("hosting-lint: new direct clock, timer, goroutine or randomness use on a router path.")
		fmt.Println("Route it through hosting.Runtime (pkg/common/hosting), or, if it cannot affect a")
		fmt.Println("routing decision, annotate the line //blis:wallclock-ok or update the baseline on review.")
		for _, f := range failures {
			fmt.Println("  " + f)
		}
		os.Exit(1)
	}
	fmt.Printf("hosting-lint: %d files checked, no new direct uses\n", len(counts))
}

// scan counts, per file and kind, the direct uses that bypass the hosting runtime.
func scan() map[string]int {
	counts := map[string]int{}
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			for _, ex := range excluded {
				if strings.HasPrefix(path, ex) {
					return nil
				}
			}
			for kind, n := range scanFile(path) {
				counts[path+" "+kind] = n
			}
			return nil
		})
	}
	return counts
}

var forbidden = map[string]map[string]bool{
	"time":    {"Now": true, "Since": true, "Until": true, "NewTicker": true, "NewTimer": true, "AfterFunc": true, "After": true, "Tick": true, "Sleep": true},
	"context": {"WithTimeout": true, "WithDeadline": true, "WithTimeoutCause": true, "WithDeadlineCause": true},
	"uuid":    {"New": true, "NewString": true, "NewRandom": true},
}

func scanFile(path string) map[string]int {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hosting-lint: %v\n", err)
		os.Exit(2)
	}
	exempt := map[int]bool{}
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			if strings.Contains(c.Text, "blis:wallclock-ok") {
				exempt[fset.Position(c.Pos()).Line] = true
			}
		}
	}
	pkgOf := map[string]string{} // local import name -> import path
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		name := filepath.Base(p)
		if p == "math/rand/v2" {
			name = "rand"
		}
		if imp.Name != nil {
			name = imp.Name.Name
		}
		pkgOf[name] = p
	}
	counts := map[string]int{}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.GoStmt:
			if !exempt[fset.Position(n.Pos()).Line] {
				counts["go-statement"]++
			}
		case *ast.SelectorExpr:
			id, ok := n.X.(*ast.Ident)
			if !ok || exempt[fset.Position(n.Pos()).Line] {
				return true
			}
			path := pkgOf[id.Name]
			switch {
			case path == "time" && forbidden["time"][n.Sel.Name],
				path == "context" && forbidden["context"][n.Sel.Name],
				strings.HasSuffix(path, "/uuid") && forbidden["uuid"][n.Sel.Name]:
				counts[id.Name+"."+n.Sel.Name]++
			case (path == "math/rand" || path == "math/rand/v2") && !strings.HasPrefix(n.Sel.Name, "New") &&
				n.Sel.Name != "Rand" && n.Sel.Name != "Source" && n.Sel.Name != "PCG":
				counts["rand."+n.Sel.Name]++
			}
		}
		return true
	})
	return counts
}

func readBaseline() map[string]int {
	base := map[string]int{}
	f, err := os.Open(baselinePath)
	if err != nil {
		return base
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.LastIndexByte(line, ' ')
		n, _ := strconv.Atoi(line[i+1:])
		base[line[:i]] = n
	}
	return base
}

func writeBaseline(counts map[string]int) {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("# Direct clock, timer, goroutine and randomness uses on router paths that a review\n")
	b.WriteString("# accepted because they cannot affect a routing decision in an embedded router.\n")
	b.WriteString("# Regenerate with: go run ./hack/hosting-lint -update\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "%s %d\n", k, counts[k])
	}
	if err := os.WriteFile(baselinePath, []byte(b.String()), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}

// checkBudget compares the lines the fork adds or deletes in files that exist upstream with the
// budget. New files cost nothing to re-apply; edits to upstream files can conflict on every
// release.
func checkBudget() int {
	cfg := map[string]string{}
	b, err := os.ReadFile(budgetPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok && !strings.HasPrefix(line, "#") {
			cfg[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	base, limit := cfg["upstream_base"], cfg["max_changed_upstream_lines"]
	max, _ := strconv.Atoi(limit)
	out, err := exec.Command("git", "diff", "--numstat", "--diff-filter=M", base+"...HEAD").Output()
	if err != nil {
		fmt.Fprintf(os.Stderr, "git diff %s...HEAD: %v\n", base, err)
		return 2
	}
	total := 0
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || strings.HasSuffix(fields[2], "_test.go") {
			continue
		}
		add, _ := strconv.Atoi(fields[0])
		del, _ := strconv.Atoi(fields[1])
		total += add + del
		files = append(files, fmt.Sprintf("%5d %s", add+del, fields[2]))
	}
	sort.Sort(sort.Reverse(sort.StringSlice(files)))
	fmt.Printf("patch budget: %d of %d changed lines in %d upstream files (base %s)\n", total, max, len(files), base)
	for _, f := range files {
		fmt.Println("  " + f)
	}
	if total > max {
		fmt.Println("patch budget exceeded: move code into new files, or raise the budget deliberately")
		return 1
	}
	return 0
}
