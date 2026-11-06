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
// as merge commits (BLIS design RTR-1). Every commit written for BLIS is signed, carries a DCO
// Signed-off-by line from a person, names agent help in at most one Assisted-by line, and names
// no tool in a Co-authored-by or Signed-off-by line (X20). Commits reachable from the upstream
// release that hack/hosting-lint/budget.txt names as upstream_base are upstream's, kept as
// upstream wrote them, and are not checked.
//
//	fork-policy history -base <sha> -head <sha>
//	    a pull request into llm-d-r: head contains base; head's upstream_base is a tagged
//	    upstream release that head contains and that contains base's upstream_base; and head's
//	    go.mod declares the go version that release declares (LIF-2)
//	fork-policy landed -before <sha> -after <sha>
//	    a push to llm-d-r: after is a merge commit whose first parent is before, plus the
//	    history checks between them
//	fork-policy toolchain
//	    the go line of go.mod is no higher than contracts/toolchain of the blis-substrate version
//	    go.mod requires, and the go command running is exactly that toolchain (LIF-2)
//	fork-policy commits -base <sha> -head <sha> [-signatures github|git] [-repo owner/name]
//	                    [-release-bot <email>]
//	    the commit policy over the commits head adds to base, upstream's excepted. While the
//	    pinned blis-substrate's .github/x20-deferral.yaml holds, an unsigned commit and a missing
//	    Signed-off-by line are printed as deferred and do not fail; see deferral.go
//
// Each subcommand prints one line per finding and exits 1 when there is any, 2 on a usage or git
// error.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/version"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
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
	fmt.Fprintln(os.Stderr, "usage: fork-policy history|landed|toolchain|commits [flags]; see the package comment")
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
	case "commits":
		base := fs.String("base", "", "the tip of llm-d-r the pull request merges into")
		head := fs.String("head", "", "the pull request's head commit")
		sigs := fs.String("signatures", "github", "how signatures are verified: github (the commit's verification on GitHub) or git (git's own verification, with the signing keys configured)")
		repo := fs.String("repo", os.Getenv("GITHUB_REPOSITORY"), "owner/name on GitHub, for -signatures github")
		bot := fs.String("release-bot", "", "the release bot's commit author email; its commits must be signed and carry no sign-off")
		if err := parse(fs, args, "base", "head"); err != nil {
			return nil, err
		}
		var v verifier
		switch *sigs {
		case "github":
			if *repo == "" {
				return nil, errors.New("-signatures github needs -repo owner/name (or GITHUB_REPOSITORY)")
			}
			v = githubVerifier(*repo)
		case "git":
			v = g.verified
		default:
			return nil, fmt.Errorf("-signatures %q: want github or git", *sigs)
		}
		found, err := commits(g, *base, *head, v, *bot)
		if err != nil {
			return nil, err
		}
		// The deferral file is read from the blis-substrate commit go.mod pins (X20).
		src, err := readDeferral(dir)
		if err != nil {
			return nil, err
		}
		fail, deferred := applyDeferral(src, time.Now().UTC(), found)
		for _, d := range deferred {
			fmt.Println("deferred:", d)
		}
		return fail, nil
	default:
		return nil, fmt.Errorf("unknown subcommand %q: want history, landed, toolchain or commits", cmd)
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
	if ok, err := g.isAncestor(oldBase, newBase); err != nil {
		return nil, err
	} else if !ok {
		out = append(out, fmt.Sprintf("upstream_base moves from %s (%s) to %s (%s), which does not contain it: "+
			"an upstream release is merged forward, never replaced (RTR-1)", short(oldBase), oldTag, short(newBase), newTag))
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

// substrateModule is the module whose contracts/toolchain names the one Go toolchain of BLIS.
const substrateModule = "github.com/inference-sim/blis-substrate"

// substrateDir returns the directory of the blis-substrate version the module in dir requires,
// downloading it when the module cache lacks it.
func substrateDir(dir string) (string, error) {
	modDir, err := goOutput(dir, "list", "-m", "-f", "{{.Dir}}", substrateModule)
	if err != nil {
		return "", err
	}
	if modDir != "" {
		return modDir, nil
	}
	cmdOut, err := goOutput(dir, "mod", "download", "-json", substrateModule)
	if err != nil {
		return "", err
	}
	var m struct{ Dir string }
	if err := json.Unmarshal([]byte(cmdOut), &m); err != nil || m.Dir == "" {
		return "", fmt.Errorf("go mod download %s: no module directory in %q", substrateModule, cmdOut)
	}
	return m.Dir, nil
}

// toolchain checks the module in dir against contracts/toolchain of the blis-substrate version
// its go.mod requires.
func toolchain(dir string) ([]string, error) {
	g := git{dir: dir}
	goLine, err := goDirective(g, "HEAD")
	if err != nil {
		return nil, err
	}
	modDir, err := substrateDir(dir)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(modDir, "contracts", "toolchain"))
	if err != nil {
		return nil, fmt.Errorf("the pinned %s has no contracts/toolchain: %w", substrateModule, err)
	}
	running, err := goOutput(dir, "env", "GOVERSION")
	if err != nil {
		return nil, err
	}
	return compareToolchain(goLine, running, strings.TrimSpace(string(b)))
}

// compareToolchain returns the findings for a go line goLine (as go.mod writes it, 1.N.M) and a
// running go command running (go1.N.M), against the toolchain named in contracts/toolchain.
func compareToolchain(goLine, running, want string) ([]string, error) {
	if !version.IsValid(want) {
		return nil, fmt.Errorf("contracts/toolchain names %q, not a Go toolchain version go1.N.M", want)
	}
	if !version.IsValid("go" + goLine) {
		return nil, fmt.Errorf("go.mod's go line %q is not a Go version", goLine)
	}
	var out []string
	if version.Compare("go"+goLine, want) > 0 {
		out = append(out, fmt.Sprintf("go.mod declares go %s, above contracts/toolchain %s: the release bot raises "+
			"contracts/toolchain first, in the same review set (LIF-2)", goLine, want))
	}
	if running != want {
		out = append(out, fmt.Sprintf("the go command is %s, not contracts/toolchain %s: install exactly that version "+
			"and set GOTOOLCHAIN=local (LIF-2)", running, want))
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

// verifier reports whether a commit's signature is verified.
type verifier func(sha string) (bool, error)

// commits checks the commit policy (X20) on the commits head adds to base, except those
// reachable from head's upstream_base, which are upstream's.
func commits(g git, base, head string, verified verifier, releaseBot string) ([]string, error) {
	upstream, _, err := upstreamBase(g, head)
	if err != nil {
		return nil, err
	}
	list, err := g.lines("rev-list", "--reverse", head, "^"+base, "^"+upstream)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, sha := range list {
		c, err := g.commit(sha)
		if err != nil {
			return nil, err
		}
		ok, err := verified(sha)
		if err != nil {
			return nil, err
		}
		for _, f := range policy(c, ok, releaseBot) {
			out = append(out, fmt.Sprintf("%s %q: %s", short(sha), c.subject, f))
		}
	}
	return out, nil
}

// commitInfo is what the policy reads from one commit.
type commitInfo struct {
	subject, authorEmail string
	trailers             [][2]string // key, value, in order, as git parses them
}

// policy returns each way commit c breaks X20. signed is whether its signature is verified.
func policy(c commitInfo, signed bool, releaseBot string) []string {
	var out []string
	if !signed {
		out = append(out, "it is not signed with a verified signature (X20)")
	}
	var signoffs, assisted []string
	for _, t := range c.trailers {
		switch strings.ToLower(t[0]) {
		case "signed-off-by":
			signoffs = append(signoffs, t[1])
			if namesTool(t[1]) {
				out = append(out, fmt.Sprintf("Signed-off-by %q names a tool or bot: only a person certifies the DCO (X20)", t[1]))
			}
		case "co-authored-by":
			if namesTool(t[1]) {
				out = append(out, fmt.Sprintf("Co-authored-by %q names a tool: say so in one Assisted-by line instead (X20)", t[1]))
			}
		case "assisted-by":
			assisted = append(assisted, t[1])
		}
	}
	if releaseBot != "" && strings.EqualFold(c.authorEmail, releaseBot) {
		if len(signoffs) > 0 {
			out = append(out, "a release-bot commit carries a Signed-off-by line; the bot never signs off (X20)")
		}
		return out
	}
	if len(signoffs) == 0 {
		out = append(out, "it has no Signed-off-by line from the person who takes responsibility for it (X20)")
	}
	if len(assisted) > 1 {
		out = append(out, fmt.Sprintf("it has %d Assisted-by lines; agent help is named in one (X20)", len(assisted)))
	}
	for _, a := range assisted {
		if strings.TrimSpace(a) == "" {
			out = append(out, "its Assisted-by line names no tool (X20)")
		}
	}
	return out
}

// toolEmail matches the commit identities of coding agents and GitHub Apps. A trailer naming one
// is not a person.
var toolEmail = regexp.MustCompile(`(?i)<[^>]*(@anthropic\.com|@openai\.com|@cursor\.com|copilot@users\.noreply\.github\.com|\[bot\]@users\.noreply\.github\.com)>`)

// namesTool reports whether a trailer value ("Name <email>") is an agent's or a bot's identity,
// judged by its email address only, so a person's name is never mistaken for a tool's.
func namesTool(v string) bool { return toolEmail.MatchString(v) }

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

func (g git) commit(sha string) (commitInfo, error) {
	head, err := g.text("show", "-s", "--format=%ae%n%s", sha)
	if err != nil {
		return commitInfo{}, err
	}
	email, subject, _ := strings.Cut(head, "\n")
	raw, err := g.lines("show", "-s", "--format=%(trailers:only,unfold)", sha)
	if err != nil {
		return commitInfo{}, err
	}
	c := commitInfo{subject: subject, authorEmail: email}
	for _, line := range raw {
		if k, v, ok := strings.Cut(line, ":"); ok {
			c.trailers = append(c.trailers, [2]string{strings.TrimSpace(k), strings.TrimSpace(v)})
		}
	}
	return c, nil
}

// verified is git's own verdict on the commit's signature: G is a good signature from a key the
// configuration trusts.
func (g git) verified(sha string) (bool, error) {
	s, err := g.text("show", "-s", "--format=%G?", sha)
	return s == "G", err
}

// githubVerifier asks GitHub whether a commit's signature is verified, as the pull request page
// shows it.
func githubVerifier(repo string) verifier {
	return func(sha string) (bool, error) {
		out, err := exec.Command("gh", "api", "repos/"+repo+"/commits/"+sha, "--jq", ".commit.verification.verified").Output()
		if err != nil {
			return false, fmt.Errorf("gh api repos/%s/commits/%s: %w", repo, short(sha), err)
		}
		switch strings.TrimSpace(string(out)) {
		case "true":
			return true, nil
		case "false":
			return false, nil
		default:
			return false, fmt.Errorf("gh api repos/%s/commits/%s: unexpected verification %q", repo, short(sha), out)
		}
	}
}
