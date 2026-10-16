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
// Every clock read, timer, deadline, goroutine, random draw, iteration-order dependence and
// outbound HTTP call on a router path must go through hosting.Runtime, or a simulator that embeds
// the router loses determinism. The lint type-checks the router packages and records each direct
// use individually, keyed by file, kind, enclosing function and the use's source line. It fails
// when a use is not in the reviewed baseline (or occurs more often than the baseline records), so
// a rebase onto a new release cannot trade a removed use for a new one. A new use must either be
// converted or, on review, added to the baseline. A line annotated "//blis:wallclock-ok" is not
// counted.
//
// It also enforces the patch budget: the lines the fork changes in files it shares with
// upstream, so the cost of re-applying the fork on each release stays bounded.
//
//	go run ./hack/hosting-lint            check against hack/hosting-lint/baseline.txt
//	go run ./hack/hosting-lint -update    rewrite the baseline
//	go run ./hack/hosting-lint -budget    check the patch budget against upstream_base
package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/tools/go/packages"
)

var roots = []string{"pkg/epp", "pkg/kvcache", "pkg/kvevents", "pkg/common", "cmd/epp/runner"}

// excluded are paths whose code never runs in an embedded router.
var excluded = []string{
	"pkg/common/hosting/",        // the seam itself
	"pkg/epp/server/",            // ext_proc, health and metrics servers
	"pkg/epp/handlers/server.go", // the gRPC stream loop; the embedder drives the handlers
	"pkg/epp/controller/",        // Kubernetes reconcilers run by a controller manager
	"pkg/epp/framework/plugins/datalayer/source/mocks/",
	"pkg/epp/framework/interface/flowcontrol/mocks/",
	"pkg/epp/datalayer/mocks/",
	"pkg/epp/framework/plugins/requestcontrol/dataproducer/predictedlatency/latencypredictorclient/tests/",
}

const (
	baselinePath = "hack/hosting-lint/baseline.txt"
	budgetPath   = "hack/hosting-lint/budget.txt"
	exemptMarker = "blis:wallclock-ok"
	pickerPkg    = "github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/scheduling/picker"
)

// use identifies one direct use. Identical uses in the same function are counted, not merged away.
type use struct {
	file, fn, kind, text string
}

func (u use) String() string { return u.file + "\t" + u.fn + "\t" + u.kind + "\t" + u.text }

func main() {
	update := flag.Bool("update", false, "rewrite the baseline")
	budget := flag.Bool("budget", false, "check the patch budget")
	flag.Parse()
	if *budget {
		os.Exit(checkBudget())
	}
	files := listFiles()
	uses := scan(files)
	total := 0
	for _, n := range uses {
		total += n
	}
	if *update {
		writeBaseline(uses)
		fmt.Printf("hosting-lint: %d files scanned, %d direct uses written to %s (%d entries)\n",
			len(files), total, baselinePath, len(uses))
		return
	}
	base := readBaseline()
	var failures, stale []string
	for u, n := range uses {
		if n > base[u] {
			failures = append(failures, fmt.Sprintf("%s:%s %s: %q (%d use(s), baseline %d)", u.file, u.fn, u.kind, u.text, n, base[u]))
		}
	}
	for u, n := range base {
		if n > uses[u] {
			stale = append(stale, fmt.Sprintf("%s:%s %s: %q (baseline %d, found %d)", u.file, u.fn, u.kind, u.text, n, uses[u]))
		}
	}
	sort.Strings(failures)
	sort.Strings(stale)
	if len(stale) > 0 {
		fmt.Printf("hosting-lint: note: %d baseline entries no longer match a use (stale; regenerate with -update):\n", len(stale))
		for _, s := range stale {
			fmt.Println("  " + s)
		}
	}
	if len(failures) > 0 {
		fmt.Println("hosting-lint: new direct clock, timer, goroutine, randomness, map-order or HTTP use on a router path.")
		fmt.Println("Route it through hosting.Runtime (pkg/common/hosting). If it cannot affect a routing decision,")
		fmt.Println("or runs only in a plugin the embedder refuses, annotate the line //blis:wallclock-ok or add it")
		fmt.Println("to the baseline on review (go run ./hack/hosting-lint -update).")
		for _, f := range failures {
			fmt.Println("  " + f)
		}
		os.Exit(1)
	}
	fmt.Printf("hosting-lint: %d files scanned, %d direct uses, all in the baseline\n", len(files), total)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "hosting-lint: "+format+"\n", args...)
	os.Exit(2)
}

// listFiles returns the non-test Go files under the roots, minus the excluded paths. A missing
// root (for example one upstream renamed) or any walk error is fatal, so nothing is silently
// skipped.
func listFiles() []string {
	if _, err := os.Stat("go.mod"); err != nil {
		fatalf("run from the repository root: %v", err)
	}
	var files []string
	for _, root := range roots {
		info, err := os.Stat(root)
		if err != nil {
			fatalf("root %s: %v (was it renamed upstream? update roots in hack/hosting-lint/main.go)", root, err)
		}
		if !info.IsDir() {
			fatalf("root %s is not a directory", root)
		}
		err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || isExcluded(path) {
				return nil
			}
			files = append(files, filepath.ToSlash(path))
			return nil
		})
		if err != nil {
			fatalf("walking %s: %v", root, err)
		}
	}
	sort.Strings(files)
	return files
}

func isExcluded(path string) bool {
	path = filepath.ToSlash(path)
	for _, ex := range excluded {
		if strings.HasPrefix(path, ex) {
			return true
		}
	}
	return false
}

// scan type-checks the packages under the roots and counts the direct uses in files.
func scan(files []string) map[use]int {
	wd, err := os.Getwd()
	if err != nil {
		fatalf("%v", err)
	}
	want := map[string]bool{}
	for _, f := range files {
		want[f] = true
	}
	patterns := make([]string, 0, len(roots))
	for _, r := range roots {
		patterns = append(patterns, "./"+r+"/...")
	}
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo,
		Dir:  wd,
	}
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		fatalf("loading packages: %v", err)
	}
	if n := packages.PrintErrors(pkgs); n > 0 {
		fatalf("%d error(s) loading packages; the lint needs the router to type-check", n)
	}
	uses := map[use]int{}
	seen := map[string]bool{}
	for _, pkg := range pkgs {
		for _, f := range pkg.Syntax {
			abs := pkg.Fset.Position(f.Pos()).Filename
			rel, err := filepath.Rel(wd, abs)
			if err != nil {
				fatalf("%v", err)
			}
			rel = filepath.ToSlash(rel)
			if !want[rel] || seen[rel] {
				continue
			}
			seen[rel] = true
			src, err := os.ReadFile(abs)
			if err != nil {
				fatalf("%v", err)
			}
			scanFile(pkg, f, rel, src, uses)
		}
	}
	var missing []string
	for _, f := range files {
		if !seen[f] {
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		fatalf("%d file(s) under the roots were not type-checked (build constraints?): %s",
			len(missing), strings.Join(missing, ", "))
	}
	return uses
}

func scanFile(pkg *packages.Package, f *ast.File, rel string, src []byte, uses map[use]int) {
	fset, info := pkg.Fset, pkg.TypesInfo
	lines := bytes.Split(src, []byte("\n"))
	exempt := map[int]bool{}
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			if strings.Contains(c.Text, exemptMarker) {
				exempt[fset.Position(c.Pos()).Line] = true
			}
		}
	}
	record := func(pos token.Pos, kind string) {
		line := fset.Position(pos).Line
		if exempt[line] {
			return
		}
		text := ""
		if line-1 < len(lines) {
			text = strings.Join(strings.Fields(string(lines[line-1])), " ")
		}
		uses[use{file: rel, fn: enclosing(f, pos), kind: kind, text: text}]++
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.GoStmt:
			record(n.Pos(), "go-statement")
		case *ast.RangeStmt:
			if t := info.TypeOf(n.X); t != nil {
				if _, ok := t.Underlying().(*types.Map); ok {
					record(n.Pos(), "range-map")
				}
			}
		case *ast.Ident:
			if kind := classify(info.Uses[n]); kind != "" {
				record(n.Pos(), kind)
			}
		}
		return true
	})
}

var pkgFuncs = map[string]map[string]bool{
	"time":                              {"Now": true, "Since": true, "Until": true, "NewTicker": true, "NewTimer": true, "AfterFunc": true, "After": true, "Tick": true, "Sleep": true},
	"context":                           {"WithTimeout": true, "WithDeadline": true, "WithTimeoutCause": true, "WithDeadlineCause": true},
	"github.com/google/uuid":            {"New": true, "NewString": true, "NewRandom": true},
	"net/http":                          {"Get": true, "Post": true, "Head": true, "PostForm": true},
	"github.com/jellydator/ttlcache/v3": {"New": true},
	"maps":                              {"Keys": true, "Values": true, "All": true},
}

// methods are flagged by "pkgpath.Type" of the (dereferenced) receiver.
var methods = map[string]map[string]bool{
	"sync.WaitGroup":                          {"Go": true},
	"sync.Map":                                {"Range": true},
	"golang.org/x/sync/errgroup.Group":        {"Go": true, "TryGo": true},
	"net/http.Client":                         {"Do": true, "Get": true, "Post": true, "Head": true, "PostForm": true},
	"github.com/jellydator/ttlcache/v3.Cache": {"Start": true},
}

// classify returns the kind of direct use obj denotes, or "" if it is not one. Objects of the
// hosting runtime (methods on hosting.Runtime, hosting.Now and hosting.Since) are never flagged.
func classify(obj types.Object) string {
	if obj == nil || obj.Pkg() == nil {
		return ""
	}
	path := obj.Pkg().Path()
	switch obj := obj.(type) {
	case *types.Func:
		obj = obj.Origin()
		sig, _ := obj.Type().(*types.Signature)
		if sig == nil {
			return ""
		}
		if recv := sig.Recv(); recv != nil {
			t := recv.Type()
			if p, ok := t.(*types.Pointer); ok {
				t = p.Elem()
			}
			named, ok := t.(*types.Named)
			if !ok {
				return "" // interface methods (hosting.Runtime included) are not flagged
			}
			tn := named.Origin().Obj()
			if tn.Pkg() == nil {
				return ""
			}
			if methods[tn.Pkg().Path()+"."+tn.Name()][obj.Name()] {
				return tn.Pkg().Name() + "." + tn.Name() + "." + obj.Name()
			}
			return ""
		}
		if pkgFuncs[path][obj.Name()] {
			return obj.Pkg().Name() + "." + obj.Name()
		}
		if (path == "math/rand" || path == "math/rand/v2") && !strings.HasPrefix(obj.Name(), "New") {
			return "rand." + obj.Name()
		}
	case *types.TypeName:
		if path == "k8s.io/utils/clock" && obj.Name() == "RealClock" {
			return "clock.RealClock"
		}
	case *types.Var:
		if obj.IsField() || obj.Parent() != obj.Pkg().Scope() {
			return ""
		}
		if path == "net/http" && obj.Name() == "DefaultClient" {
			return "http.DefaultClient"
		}
		if path == pickerPkg && obj.Name() == "PickerRand" {
			return "picker.PickerRand"
		}
	}
	return ""
}

// enclosing names the top-level declaration containing pos: "Func", "Recv.Method", or "<file>".
func enclosing(f *ast.File, pos token.Pos) string {
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || pos < fd.Pos() || pos >= fd.End() {
			continue
		}
		if fd.Recv == nil || len(fd.Recv.List) == 0 {
			return fd.Name.Name
		}
		return recvName(fd.Recv.List[0].Type) + "." + fd.Name.Name
	}
	return "<file>"
}

func recvName(e ast.Expr) string {
	switch e := e.(type) {
	case *ast.StarExpr:
		return recvName(e.X)
	case *ast.IndexExpr:
		return recvName(e.X)
	case *ast.IndexListExpr:
		return recvName(e.X)
	case *ast.ParenExpr:
		return recvName(e.X)
	case *ast.Ident:
		return e.Name
	}
	return "?"
}

// The baseline has one tab-separated entry per line: file, function, kind, count, line text.
func readBaseline() map[use]int {
	base := map[use]int{}
	f, err := os.Open(baselinePath)
	if err != nil {
		if os.IsNotExist(err) {
			return base
		}
		fatalf("%v", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for ln := 1; sc.Scan(); ln++ {
		line := sc.Text()
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "\t", 5)
		if len(parts) != 5 {
			fatalf("%s:%d: want 5 tab-separated fields (file, function, kind, count, text)", baselinePath, ln)
		}
		n, err := strconv.Atoi(parts[3])
		if err != nil || n < 1 {
			fatalf("%s:%d: bad count %q", baselinePath, ln, parts[3])
		}
		base[use{file: parts[0], fn: parts[1], kind: parts[2], text: parts[4]}] += n
	}
	if err := sc.Err(); err != nil {
		fatalf("%s: %v", baselinePath, err)
	}
	return base
}

func writeBaseline(uses map[use]int) {
	keys := make([]use, 0, len(uses))
	for u := range uses {
		keys = append(keys, u)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	var b strings.Builder
	b.WriteString(`# Direct clock, timer, deadline, goroutine, randomness, map-iteration-order and outbound HTTP
# uses on router paths that a review accepted. An entry is accepted for one of two reasons:
#   - it cannot affect a routing decision in an embedded router (a latency metric, a log rate
#     limit, a debug dump, an order-insensitive map walk), or
#   - it runs only in a plugin or path the embedder refuses: the BLIS simulator rejects
#     unsupported plugins at startup (sim/llmd/support.go), so their code never runs hosted.
# It is NOT a claim that every entry is harmless in every embedding; a new embedder that admits
# one of these plugins must convert its entries to hosting.Runtime first.
#
# One entry per distinct use: file, enclosing function ("<file>" for package scope), kind, count
# of identical uses, and the use's source line (whitespace-collapsed), tab-separated. Line
# numbers are deliberately absent, so unrelated edits do not churn the baseline.
# Regenerate with: go run ./hack/hosting-lint -update
`)
	for _, u := range keys {
		fmt.Fprintf(&b, "%s\t%s\t%s\t%d\t%s\n", u.file, u.fn, u.kind, uses[u], u.text)
	}
	if err := os.WriteFile(baselinePath, []byte(b.String()), 0o644); err != nil {
		fatalf("%v", err)
	}
}

// checkBudget compares the lines the fork adds or deletes in files that exist upstream with the
// budget. New files cost nothing to re-apply; edits to, renames of and deletions of upstream
// files can conflict on every release. A rename costs at least one line even when the content is
// unchanged.
func checkBudget() int {
	cfg := map[string]string{}
	b, err := os.ReadFile(budgetPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if k, v, ok := strings.Cut(line, "="); ok && !strings.HasPrefix(line, "#") {
			cfg[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	base, limit := cfg["upstream_base"], cfg["max_changed_upstream_lines"]
	maxLines, err := strconv.Atoi(limit)
	if base == "" || err != nil {
		fmt.Fprintf(os.Stderr, "%s: need upstream_base and an integer max_changed_upstream_lines\n", budgetPath)
		return 2
	}
	out, err := exec.Command("git", "diff", "--numstat", "-z", "-M", "--diff-filter=MRD", base+"...HEAD").Output()
	if err != nil {
		fmt.Fprintf(os.Stderr, "git diff %s...HEAD: %v (is upstream_base fetched? CI needs full history)\n", base, err)
		return 2
	}
	total := 0
	var files []string
	// With -z each record is "add\tdel\tpath\0", or "add\tdel\t\0old\0new\0" for a rename.
	recs := strings.Split(string(out), "\x00")
	for i := 0; i < len(recs); i++ {
		fields := strings.SplitN(recs[i], "\t", 3)
		if len(fields) != 3 {
			continue
		}
		path, label, rename := fields[2], "", false
		if path == "" && i+2 < len(recs) {
			path, label, rename = recs[i+1], " -> "+recs[i+2], true
			i += 2
		}
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		add, errA := strconv.Atoi(fields[0])
		del, errD := strconv.Atoi(fields[1])
		cost := add + del
		if errA != nil || errD != nil { // binary file
			cost = 1
		}
		if rename && cost == 0 {
			cost = 1
		}
		total += cost
		files = append(files, fmt.Sprintf("%5d %s%s", cost, path, label))
	}
	sort.Sort(sort.Reverse(sort.StringSlice(files)))
	fmt.Printf("patch budget: %d of %d changed lines in %d upstream files (base %s)\n", total, maxLines, len(files), base)
	for _, f := range files {
		fmt.Println("  " + f)
	}
	if total > maxLines {
		fmt.Println("patch budget exceeded: move code into new files, or raise the budget deliberately")
		return 1
	}
	return 0
}
