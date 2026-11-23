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

// Command fork-policy checks the history rules of the inference-sim fork's llm-d-r branch.
//
// Upstream llm-d releases are merged into llm-d-r and never rebased, and fork pull requests land
// as merge commits (BLIS design RTR-1).
//
//	fork-policy history -base <sha> -head <sha>
//	    a pull request into llm-d-r: head contains base; head's upstream_base is a tagged
//	    upstream release that head contains, head still contains base's upstream_base, head holds
//	    no upstream commit outside those two releases (upstream's branches must be fetched as
//	    refs/remotes/upstream/*); and head's go.mod declares the go version that release declares (LIF-2)
//	fork-policy landed -before <sha> -after <sha>
//	    a push to llm-d-r: after is a merge commit whose first parent is before, plus the
//	    history checks between them
//	fork-policy toolchain
//	    the go line of go.mod is no higher than the toolchain in hack/fork-policy/toolchain, and
//	    the go command running is exactly that toolchain (LIF-2)
//
// Each subcommand prints one line per finding and exits 1 when there is any, 2 on a usage or git
// error.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"go/version"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

const budgetPath = "hack/hosting-lint/budget.txt"

// releaseTag is the form of an upstream release tag. Release candidates and the fork's own
// vX.Y.Z-blis.N tags do not match.
var releaseTag = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	findings, err := run(".", os.Args[1], os.Args[2:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "fork-policy:", err)
		os.Exit(2)
	}
	for _, f := range findings {
		fmt.Println(f)
	}
	if len(findings) > 0 {
		os.Exit(1)
	}
	fmt.Printf("fork-policy %s: no findings\n", os.Args[1])
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: fork-policy history|landed|toolchain [flags]; see the package comment")
	os.Exit(2)
}

// run parses the subcommand's flags and runs it in the repository at dir.
func run(dir, cmd string, args []string) ([]string, error) {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	g := git{dir: dir}
	switch cmd {
	case "history":
		base := fs.String("base", "", "the tip of llm-d-r the pull request merges into")
		head := fs.String("head", "", "the pull request's head commit")
		if err := parse(fs, args, "base", "head"); err != nil {
			return nil, err
		}
		return history(g, *base, *head)
	case "landed":
		before := fs.String("before", "", "llm-d-r's tip before the push")
		after := fs.String("after", "", "llm-d-r's tip after the push")
		if err := parse(fs, args, "before", "after"); err != nil {
			return nil, err
		}
		return landed(g, *before, *after)
	case "toolchain":
		if err := parse(fs, args); err != nil {
			return nil, err
		}
		return toolchain(dir)
	default:
		return nil, fmt.Errorf("unknown subcommand %q: want history, landed or toolchain", cmd)
	}
}

// parse parses args into fs and refuses positional arguments and an empty value of any flag
// named in required.
func parse(fs *flag.FlagSet, args []string, required ...string) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	var missing []string
	for _, name := range required {
		if fs.Lookup(name).Value.String() == "" {
			missing = append(missing, "-"+name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s: missing %s", fs.Name(), strings.Join(missing, ", "))
	}
	return nil
}

// history checks a pull request into llm-d-r: it keeps llm-d-r's history, and the upstream
// release its budget.txt names is merged into it, never rebased.
func history(g git, base, head string) ([]string, error) {
	base, head, err := g.resolve2(base, head)
	if err != nil {
		return nil, err
	}
	var out []string
	ok, err := g.isAncestor(base, head)
	if err != nil {
		return nil, err
	}
	if !ok {
		out = append(out, fmt.Sprintf("head %s does not contain llm-d-r's tip %s: the branch rewrites llm-d-r's history; "+
			"merge upstream releases and llm-d-r into it, never rebase (RTR-1)", short(head), short(base)))
	}
	newBase, newTag, err := upstreamBase(g, head)
	if err != nil {
		return nil, err
	}
	oldBase, oldTag, err := upstreamBase(g, base)
	if err != nil {
		return nil, err
	}
	if ok, err := g.isAncestor(newBase, head); err != nil {
		return nil, err
	} else if !ok {
		out = append(out, fmt.Sprintf("head %s does not contain its upstream_base %s (%s): merge the release with "+
			"git merge --no-ff %s (RTR-1)", short(head), short(newBase), newTag, newTag))
	}
	// Upstream cuts each release on its own release branch, so a release does not contain the
	// one before it. The rule is that the head keeps the release it had.
	if ok, err := g.isAncestor(oldBase, head); err != nil {
		return nil, err
	} else if !ok {
		out = append(out, fmt.Sprintf("upstream_base moves from %s (%s) to %s (%s), but head %s does not contain %s: "+
			"an upstream release is merged forward, never replaced (RTR-1)", short(oldBase), oldTag, short(newBase), newTag,
			short(head), short(oldBase)))
	}
	stray, err := strayUpstream(g, base, head, oldBase, newBase)
	if err != nil {
		return nil, err
	}
	if len(stray) > 0 {
		out = append(out, fmt.Sprintf("head %s contains %d upstream commits that belong to no release (first %s): "+
			"merge the tagged release upstream_base names (%s), not upstream's branch (RTR-1)",
			short(head), len(stray), short(stray[0]), newTag))
	}
	ours, err := goDirective(g, head)
	if err != nil {
		return nil, err
	}
	theirs, err := goDirective(g, newBase)
	if err != nil {
		return nil, err
	}
	if ours != theirs {
		out = append(out, fmt.Sprintf("go.mod at %s declares go %s; the fork's go line follows upstream %s, which declares go %s (LIF-2)",
			short(head), ours, newTag, theirs))
	}
	return out, nil
}

// strayUpstream returns the commits head adds to base that upstream's branches hold and that
// neither upstream_base (old or new) contains: upstream content merged outside a release. It
// needs upstream's branches fetched as refs/remotes/upstream/* and its release tags.
func strayUpstream(g git, base, head, oldBase, newBase string) ([]string, error) {
	refs, err := g.lines("for-each-ref", "--format=%(refname)", "refs/remotes/upstream/")
	if err != nil {
		return nil, err
	}
	if len(refs) == 0 {
		return nil, errors.New("no refs/remotes/upstream/* branches: fetch upstream's branches " +
			"(git remote add upstream https://github.com/llm-d/llm-d-router.git && git fetch --no-tags upstream)")
	}
	tags, err := g.lines("for-each-ref", "--format=%(refname)", "refs/tags/v*")
	if err != nil {
		return nil, err
	}
	for _, t := range tags { // the fork's own vX.Y.Z-blis.N tags are not upstream's
		if !strings.Contains(t, "-blis") {
			refs = append(refs, t)
		}
	}
	added, err := g.lines("rev-list", head, "^"+base, "^"+oldBase, "^"+newBase)
	if err != nil || len(added) == 0 {
		return nil, err
	}
	inUpstream, err := g.lines(append([]string{"rev-list"}, append(refs, "^"+oldBase, "^"+newBase)...)...)
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(inUpstream))
	for _, c := range inUpstream {
		set[c] = true
	}
	var stray []string
	for _, c := range added {
		if set[c] {
			stray = append(stray, c)
		}
	}
	return stray, nil
}

// toolchainFile names the one Go toolchain of BLIS: a line go1.N.M. The same toolchain is named in
// inference-sim's substrate/contracts/toolchain; the fork cannot read that file (it depends on no
// BLIS module), so a change of the toolchain edits both, by hand.
const toolchainFile = "hack/fork-policy/toolchain"

// toolchain checks the module in dir against the toolchain in toolchainFile.
func toolchain(dir string) ([]string, error) {
	g := git{dir: dir}
	goLine, err := goDirective(g, "HEAD")
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(toolchainFile)))
	if err != nil {
		return nil, fmt.Errorf("the fork has no toolchain file: %w", err)
	}
	running, err := goOutput(dir, "env", "GOVERSION")
	if err != nil {
		return nil, err
	}
	return compareToolchain(goLine, running, strings.TrimSpace(string(b)))
}

// compareToolchain returns the findings for a go line goLine (as go.mod writes it, 1.N.M) and a
// running go command running (go1.N.M), against the toolchain named in toolchainFile.
func compareToolchain(goLine, running, want string) ([]string, error) {
	if !version.IsValid(want) {
		return nil, fmt.Errorf("%s names %q, not a Go toolchain version go1.N.M", toolchainFile, want)
	}
	if !version.IsValid("go" + goLine) {
		return nil, fmt.Errorf("go.mod's go line %q is not a Go version", goLine)
	}
	var out []string
	if version.Compare("go"+goLine, want) > 0 {
		out = append(out, fmt.Sprintf("go.mod declares go %s, above %s %s: raise the toolchain file first, "+
			"in the same review set as inference-sim's substrate/contracts/toolchain (LIF-2)", goLine, toolchainFile, want))
	}
	if running != want {
		out = append(out, fmt.Sprintf("the go command is %s, not %s %s: install exactly that version "+
			"and set GOTOOLCHAIN=local (LIF-2)", running, toolchainFile, want))
	}
	return out, nil
}

// goOutput runs the go command in dir and returns its trimmed standard output.
func goOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("go %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

// goDirective returns the version on the go line of go.mod at rev.
func goDirective(g git, rev string) (string, error) {
	b, err := g.output("show", rev+":go.mod")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == "go" {
			return f[1], nil
		}
	}
	return "", fmt.Errorf("go.mod at %s has no go line", short(rev))
}

// landed checks a push to llm-d-r: the pull request landed as a merge commit on top of the
// previous tip, and the history rules hold between the two tips.
func landed(g git, before, after string) ([]string, error) {
	if strings.Trim(before, "0") == "" {
		return []string{fmt.Sprintf("llm-d-r was created or recreated at %s: there is no previous tip to check "+
			"against; llm-d-r is never deleted (RTR-1)", short(after))}, nil
	}
	before, after, err := g.resolve2(before, after)
	if err != nil {
		return nil, err
	}
	out, err := history(g, before, after)
	if err != nil {
		return nil, err
	}
	parents, err := g.parents(after)
	if err != nil {
		return nil, err
	}
	if len(parents) != 2 || parents[0] != before {
		out = append(out, fmt.Sprintf("llm-d-r's new tip %s has parents %s; a fork pull request lands as a merge "+
			"commit whose first parent is the previous tip %s: a squash or rebase merge, or a direct push, "+
			"breaks RTR-1", short(after), shortAll(parents), short(before)))
	}
	return out, nil
}

// upstreamBase returns the upstream_base that rev's budget.txt records and the upstream release
// tag that points at it. A base without a release tag is an error: either the tags are not
// fetched, or the fork is based on something that is not an upstream release.
func upstreamBase(g git, rev string) (sha, tag string, err error) {
	b, err := g.output("show", rev+":"+budgetPath)
	if err != nil {
		return "", "", fmt.Errorf("reading %s at %s: %w", budgetPath, short(rev), err)
	}
	var base string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if k, v, ok := strings.Cut(line, "="); ok && !strings.HasPrefix(line, "#") && strings.TrimSpace(k) == "upstream_base" {
			base = strings.TrimSpace(v)
		}
	}
	if base == "" {
		return "", "", fmt.Errorf("%s at %s records no upstream_base", budgetPath, short(rev))
	}
	sha, err = g.text("rev-parse", "--verify", base+"^{commit}")
	if err != nil {
		return "", "", fmt.Errorf("upstream_base %s at %s is not a commit here (fetch upstream): %w", base, short(rev), err)
	}
	tags, err := g.lines("tag", "--points-at", sha)
	if err != nil {
		return "", "", err
	}
	for _, t := range tags {
		if releaseTag.MatchString(t) {
			return sha, t, nil
		}
	}
	return "", "", fmt.Errorf("upstream_base %s at %s carries no upstream release tag vX.Y.Z (fetch upstream's tags)", short(sha), short(rev))
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func shortAll(shas []string) string {
	s := make([]string, len(shas))
	for i, sha := range shas {
		s[i] = short(sha)
	}
	return "[" + strings.Join(s, " ") + "]"
}

// git runs git in one repository.
type git struct{ dir string }

func (g git) output(args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", g.dir}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func (g git) text(args ...string) (string, error) {
	out, err := g.output(args...)
	return strings.TrimSpace(string(out)), err
}

func (g git) lines(args ...string) ([]string, error) {
	s, err := g.text(args...)
	if err != nil || s == "" {
		return nil, err
	}
	return strings.Split(s, "\n"), nil
}

// resolve2 returns the full commit hashes of a and b.
func (g git) resolve2(a, b string) (string, string, error) {
	ra, err := g.text("rev-parse", "--verify", a+"^{commit}")
	if err != nil {
		return "", "", err
	}
	rb, err := g.text("rev-parse", "--verify", b+"^{commit}")
	return ra, rb, err
}

// isAncestor reports whether a is an ancestor of (or equal to) b.
func (g git) isAncestor(a, b string) (bool, error) {
	err := exec.Command("git", "-C", g.dir, "merge-base", "--is-ancestor", a, b).Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("git merge-base --is-ancestor %s %s: %w", short(a), short(b), err)
	}
	return true, nil
}

func (g git) parents(sha string) ([]string, error) {
	s, err := g.text("show", "-s", "--format=%P", sha)
	if err != nil {
		return nil, err
	}
	return strings.Fields(s), nil
}
