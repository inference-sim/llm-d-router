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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repo is a scratch git repository with no global or system configuration.
type repo struct {
	t   *testing.T
	dir string
}

func newRepo(t *testing.T) *repo {
	t.Helper()
	r := &repo{t: t, dir: t.TempDir()}
	r.git("init", "-q", "-b", "main")
	return r
}

func (r *repo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", r.dir, "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=A Person", "GIT_AUTHOR_EMAIL=person@example.com",
		"GIT_COMMITTER_NAME=A Person", "GIT_COMMITTER_EMAIL=person@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit writes content to file and commits it with message msg, returning the commit.
func (r *repo) commit(file, content, msg string) string {
	r.t.Helper()
	path := filepath.Join(r.dir, file)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
	r.git("add", file)
	r.git("commit", "-q", "-m", msg)
	return r.git("rev-parse", "HEAD")
}

// setBase records sha as upstream_base in budget.txt and commits it.
func (r *repo) setBase(sha, msg string) string {
	r.t.Helper()
	return r.commit(budgetPath, "upstream_base = "+sha+"\nmax_changed_upstream_lines = 450\n", msg)
}

func (r *repo) run(cmd string, args ...string) []string {
	r.t.Helper()
	out, err := run(r.dir, cmd, args)
	if err != nil {
		r.t.Fatalf("%s %v: %v", cmd, args, err)
	}
	return out
}

// fork builds upstream v1.0.0 and v1.1.0 on main, and llm-d-r based on v1.0.0 with one fork
// commit merged as a pull request. It returns llm-d-r's tip and the two release commits.
func fork(t *testing.T) (r *repo, tip, v100, v110 string) {
	r = newRepo(t)
	r.commit("go.mod", "module github.com/llm-d/llm-d-router\n\ngo 1.27.2\n", "upstream go.mod")
	v100 = r.commit("router.go", "package router // 1.0.0\n", "upstream 1.0.0")
	r.git("tag", "v1.0.0")
	r.commit("go.mod", "module github.com/llm-d/llm-d-router\n\ngo 1.28.0\n", "upstream raises go")
	v110 = r.commit("router.go", "package router // 1.1.0\n", "upstream 1.1.0")
	r.git("tag", "v1.1.0")
	r.git("checkout", "-q", "-b", "llm-d-r", "v1.0.0")
	r.setBase(v100, "hosting-lint: upstream_base = v1.0.0")
	r.git("checkout", "-q", "-b", "pr1")
	r.commit("hosting.go", "package hosting\n", "hosting: the seam")
	r.git("checkout", "-q", "llm-d-r")
	r.git("merge", "-q", "--no-ff", "-m", "Merge pull request #1", "pr1")
	return r, r.git("rev-parse", "HEAD"), v100, v110
}

func TestHistoryAcceptsAMergedUpstreamRelease(t *testing.T) {
	r, tip, _, v110 := fork(t)
	r.git("checkout", "-q", "-b", "upgrade")
	r.git("merge", "-q", "--no-ff", "-m", "Merge v1.1.0 into llm-d-r", "v1.1.0")
	head := r.setBase(v110, "hosting-lint: upstream_base = v1.1.0")
	if out := r.run("history", "-base", tip, "-head", head); len(out) != 0 {
		t.Fatalf("a merged upstream release: want no findings, got %q", out)
	}
}

func TestHistoryRefusesAGoLineThatDoesNotFollowUpstream(t *testing.T) {
	r, tip, _, v110 := fork(t)
	r.git("checkout", "-q", "-b", "upgrade")
	r.git("merge", "-q", "--no-ff", "-m", "Merge v1.1.0 into llm-d-r", "v1.1.0")
	r.setBase(v110, "hosting-lint: upstream_base = v1.1.0")
	head := r.commit("go.mod", "module github.com/llm-d/llm-d-router\n\ngo 1.27.2\n", "go.mod: keep go 1.27.2")
	out := r.run("history", "-base", tip, "-head", head)
	if len(out) != 1 || !strings.Contains(out[0], "declares go 1.27.2") || !strings.Contains(out[0], "declares go 1.28.0") {
		t.Fatalf("a go line below upstream's: want one LIF-2 finding, got %q", out)
	}
}

func TestHistoryRefusesARebaseOntoAnUpstreamRelease(t *testing.T) {
	r, tip, _, v110 := fork(t)
	r.git("checkout", "-q", "-b", "rebased", "v1.1.0")
	r.setBase(v110, "hosting-lint: upstream_base = v1.1.0")
	head := r.commit("hosting.go", "package hosting\n", "hosting: the seam, rebased")
	out := r.run("history", "-base", tip, "-head", head)
	if len(out) != 1 || !strings.Contains(out[0], "does not contain llm-d-r's tip") {
		t.Fatalf("a rebase onto v1.1.0: want the rewritten-history finding, got %q", out)
	}
}

func TestHistoryRefusesAnUpstreamBaseNotMerged(t *testing.T) {
	r, tip, _, v110 := fork(t)
	r.git("checkout", "-q", "-b", "claims")
	head := r.setBase(v110, "hosting-lint: upstream_base = v1.1.0, without the merge")
	out := r.run("history", "-base", tip, "-head", head)
	// Without the merge the branch also keeps v1.0.0's go line.
	if len(out) != 2 || !strings.Contains(out[0], "does not contain its upstream_base") || !strings.Contains(out[1], "LIF-2") {
		t.Fatalf("an upstream_base the branch does not contain: want the merge and go-line findings, got %q", out)
	}
}

func TestHistoryRefusesAnUpstreamBaseMovedSideways(t *testing.T) {
	r, tip, v100, _ := fork(t)
	// v1.0.1 is a patch release on a branch from v1.0.0; v1.0.2 does not contain it.
	r.git("checkout", "-q", "-b", "release-1.0", v100)
	v101 := r.commit("router.go", "package router // 1.0.1\n", "upstream 1.0.1")
	r.git("tag", "v1.0.1")
	r.git("checkout", "-q", "-b", "release-1.0-alt", v100)
	v102 := r.commit("router.go", "package router // 1.0.2\n", "upstream 1.0.2 from another line")
	r.git("tag", "v1.0.2")
	r.git("checkout", "-q", "-b", "step1", tip)
	r.git("merge", "-q", "--no-ff", "-m", "Merge v1.0.1", "v1.0.1")
	mid := r.setBase(v101, "hosting-lint: upstream_base = v1.0.1")
	r.git("merge", "-q", "--no-ff", "-X", "theirs", "-m", "Merge v1.0.2", "v1.0.2")
	head := r.setBase(v102, "hosting-lint: upstream_base = v1.0.2")
	out := r.run("history", "-base", mid, "-head", head)
	if len(out) != 1 || !strings.Contains(out[0], "never replaced") {
		t.Fatalf("an upstream_base that does not contain the previous one: want one finding, got %q", out)
	}
}

func TestHistoryRefusesAnUntaggedUpstreamBase(t *testing.T) {
	r, tip, _, _ := fork(t)
	r.git("checkout", "-q", "-b", "untagged")
	head := r.setBase(tip, "hosting-lint: upstream_base = a fork commit")
	if _, err := run(r.dir, "history", []string{"-base", tip, "-head", head}); err == nil ||
		!strings.Contains(err.Error(), "carries no upstream release tag") {
		t.Fatalf("an untagged upstream_base: want an error, got %v", err)
	}
}

func TestLandedAcceptsAMergeCommit(t *testing.T) {
	r, tip, _, _ := fork(t)
	before := r.git("rev-parse", tip+"^1")
	if out := r.run("landed", "-before", before, "-after", tip); len(out) != 0 {
		t.Fatalf("a pull request merged with a merge commit: want no findings, got %q", out)
	}
}

func TestLandedRefusesASquashOrDirectPush(t *testing.T) {
	r, tip, _, _ := fork(t)
	after := r.commit("hosting.go", "package hosting // squashed\n", "hosting: squashed pull request")
	out := r.run("landed", "-before", tip, "-after", after)
	if len(out) != 1 || !strings.Contains(out[0], "lands as a merge commit") {
		t.Fatalf("a squash merge: want one finding, got %q", out)
	}
}

func TestLandedRefusesAMergeWithTheWrongFirstParent(t *testing.T) {
	r, tip, _, _ := fork(t)
	r.git("checkout", "-q", "-b", "pr2")
	r.commit("embed.go", "package embed\n", "embed: a change")
	// The branch merges llm-d-r into itself, and that merge is pushed as llm-d-r's tip.
	r.git("merge", "-q", "--no-ff", "-m", "Merge llm-d-r into pr2", tip)
	after := r.git("rev-parse", "HEAD")
	out := r.run("landed", "-before", tip, "-after", after)
	if len(out) != 1 || !strings.Contains(out[0], "first parent is the previous tip") {
		t.Fatalf("a fast-forward to a merge whose first parent is the branch: want one finding, got %q", out)
	}
}

func TestLandedRefusesARecreatedBranch(t *testing.T) {
	r, tip, _, _ := fork(t)
	out := r.run("landed", "-before", strings.Repeat("0", 40), "-after", tip)
	if len(out) != 1 || !strings.Contains(out[0], "never deleted") {
		t.Fatalf("a created branch: want one finding, got %q", out)
	}
}

const (
	signoff  = "Signed-off-by: A Person <person@example.com>"
	assisted = "Assisted-by: Claude Code (Claude Opus 5.5)"
)

func TestPolicy(t *testing.T) {
	for _, tc := range []struct {
		name     string
		trailers []string
		signed   bool
		author   string
		want     []string // substrings, one per finding, in order
	}{
		{"person's commit", []string{signoff}, true, "", nil},
		{"agent-assisted commit", []string{assisted, signoff}, true, "", nil},
		{"unsigned", []string{signoff}, false, "", []string{"not signed"}},
		{"no sign-off", nil, true, "", []string{"no Signed-off-by"}},
		{"tool as co-author", []string{"Co-authored-by: Claude <noreply@anthropic.com>", signoff}, true, "", []string{"Co-authored-by"}},
		{"person as co-author", []string{"Co-authored-by: B Person <b@example.com>", signoff}, true, "", nil},
		{"tool signs off", []string{"Signed-off-by: Claude <noreply@anthropic.com>"}, true, "", []string{"names a tool or bot"}},
		{"bot signs off", []string{"Signed-off-by: dependabot[bot] <49699333+dependabot[bot]@users.noreply.github.com>"}, true, "", []string{"names a tool or bot"}},
		{"two Assisted-by lines", []string{assisted, assisted, signoff}, true, "", []string{"2 Assisted-by lines"}},
		{"empty Assisted-by", []string{"Assisted-by:", signoff}, true, "", []string{"names no tool"}},
		{"release bot", nil, true, "bot@example.com", nil},
		{"release bot signs off", []string{signoff}, true, "bot@example.com", []string{"release-bot commit carries a Signed-off-by"}},
		{"release bot unsigned", nil, false, "bot@example.com", []string{"not signed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := commitInfo{subject: "s", authorEmail: "person@example.com"}
			if tc.author != "" {
				c.authorEmail = tc.author
			}
			for _, tr := range tc.trailers {
				k, v, _ := strings.Cut(tr, ":")
				c.trailers = append(c.trailers, [2]string{k, strings.TrimSpace(v)})
			}
			got := policy(c, tc.signed, "bot@example.com")
			if len(got) != len(tc.want) {
				t.Fatalf("policy = %q, want %d finding(s) %q", got, len(tc.want), tc.want)
			}
			for i, w := range tc.want {
				if !strings.Contains(got[i], w) {
					t.Errorf("finding %d = %q, want it to mention %q", i, got[i], w)
				}
			}
		})
	}
}

// TestCommitsSkipsUpstreamHistory: the commit policy reads only the commits written for BLIS.
// The upstream release a pull request merges carries no sign-off and is not checked; the fork's
// own commits, including the merge commit, are.
func TestCommitsSkipsUpstreamHistory(t *testing.T) {
	r, tip, _, v110 := fork(t)
	r.git("checkout", "-q", "-b", "upgrade")
	r.git("merge", "-q", "--no-ff", "-m", "Merge v1.1.0 into llm-d-r\n\n"+signoff, "v1.1.0")
	r.setBase(v110, "hosting-lint: upstream_base = v1.1.0\n\n"+assisted+"\n"+signoff)
	head := r.commit("embed.go", "package embed\n", "embed: follow v1.1.0")
	var seen []string
	allSigned := func(sha string) (bool, error) { seen = append(seen, sha); return true, nil }
	out, err := commits(git{dir: r.dir}, tip, head, allSigned, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 3 {
		t.Errorf("checked %d commits, want the 3 the pull request wrote (not upstream's)", len(seen))
	}
	if len(out) != 1 || !strings.Contains(out[0], `"embed: follow v1.1.0"`) || !strings.Contains(out[0], "no Signed-off-by") {
		t.Fatalf("want one finding, on the commit without a sign-off, got %q", out)
	}
}

func TestRunRefusesMissingFlags(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		args []string
		want string
	}{
		{"history", []string{"-base", "x"}, "missing -head"},
		{"landed", nil, "missing -before, -after"},
		{"commits", []string{"-base", "x", "-head", "y", "-signatures", "maybe"}, `-signatures "maybe"`},
		{"commits", []string{"-base", "x", "-head", "y", "-repo", ""}, "needs -repo"},
		{"rebase", nil, "unknown subcommand"},
	} {
		t.Setenv("GITHUB_REPOSITORY", "")
		if _, err := run(t.TempDir(), tc.cmd, tc.args); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s %q: want an error mentioning %q, got %v", tc.cmd, tc.args, tc.want, err)
		}
	}
}
