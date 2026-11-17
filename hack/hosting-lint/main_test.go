package main

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/tools/go/packages"
)

// TestBareWaitGroupWaitIsAUse: a sync.WaitGroup.Wait is a use unless a Park statement precedes it
// in its own statement list and states, by "//hosting:park-joins <receiver>", that its predicate
// is the completion of the group the Wait joins (RTR-7, #2215). The fixture holds four waits that
// are uses: a bare one, one after a Park that sits elsewhere in the function, one after a Park on
// an unrelated condition, and one after a Park that names another group; and two waits that state
// the join, on the line above the Park and at its end.
func TestBareWaitGroupWaitIsAUse(t *testing.T) {
	dir, err := filepath.Abs("testdata/waitfixture")
	if err != nil {
		t.Fatal(err)
	}
	pkgs, err := packages.Load(&packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo,
		Dir:  dir,
	}, "./...")
	if err != nil {
		t.Fatal(err)
	}
	if n := packages.PrintErrors(pkgs); n > 0 {
		t.Fatalf("%d package error(s) in the fixture", n)
	}
	uses := map[use]int{}
	for _, pkg := range pkgs {
		for _, f := range pkg.Syntax {
			abs := pkg.Fset.Position(f.Pos()).Filename
			src, err := os.ReadFile(abs)
			if err != nil {
				t.Fatal(err)
			}
			scanFile(pkg, f, filepath.Base(abs), src, uses)
		}
	}
	got := map[string]int{}
	for u, n := range uses {
		if u.kind == "sync.WaitGroup.Wait" {
			got[u.fn] += n
		}
	}
	want := map[string]int{"BadBareWait": 1, "BadParkElsewhere": 1, "BadUnrelatedPark": 1, "BadParkNamesAnotherGroup": 1}
	if len(got) != len(want) {
		t.Fatalf("flagged waits %v, want %v", got, want)
	}
	for fn, n := range want {
		if got[fn] != n {
			t.Errorf("flagged waits %v, want %v", got, want)
		}
	}
}
