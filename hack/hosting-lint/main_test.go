package main

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/tools/go/packages"
)

// TestBareWaitGroupWaitIsAUse: a sync.WaitGroup.Wait is a use unless a Park statement precedes it
// in its own statement list (RTR-7). The fixture holds two waits with no such Park, one of them
// after a Park that sits elsewhere in the function, and one wait that is parked first.
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
	want := map[string]int{"BadBareWait": 1, "BadParkElsewhere": 1}
	if len(got) != len(want) {
		t.Fatalf("flagged waits %v, want %v", got, want)
	}
	for fn, n := range want {
		if got[fn] != n {
			t.Errorf("flagged waits %v, want %v", got, want)
		}
	}
}
