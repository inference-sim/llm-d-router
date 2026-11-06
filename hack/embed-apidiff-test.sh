#!/usr/bin/env bash
# Copyright 2026 The llm-d Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# embed-apidiff-test.sh runs `embed-apidiff.sh contract` in throwaway git repositories, against
# fixture contract modules served from a file module proxy, and checks that the script fails on a
# known-bad (incompatible) change and passes on the others (RTR-3). Each case is a repository with
# a fake fork tag v1.0.0-blis.1 whose go.mod pins one contract version, and a HEAD that pins
# another:
#
#   1. substrate -> substrate, a method added to an interface: reported, exit non-zero;
#   2. substrate -> substrate, a function added: passes;
#   3. the same incompatible change, but HEAD's upstream base is a new major: allowed;
#   4. tag pins blis-schemas, HEAD pins blis-substrate (the move of the contract between modules):
#      the packages are compared across modules, an incompatible change reported, a compatible
#      one passes;
#   5. the same pin on both sides: no change, nothing compared;
#   6. accept-break (--accept-break owner/repo#N, or ACCEPT_BREAK): on the known-bad change it
#      prints the change, names the review set and passes; a missing, empty or malformed id fails;
#      without the flag the known-bad change still fails; a non-API error (a contract version
#      that cannot be resolved) still fails under accept-break.
#
# The fixtures need no network. Go's module cache is a fresh directory, fed from the fixture
# proxy and the user's existing module cache (for apidiff and the Go toolchain it needs); with an
# empty user cache the apidiff download needs the network.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
script=$here/embed-apidiff.sh
work=$(mktemp -d)
realcache=$(go env GOMODCACHE)
# Pin the toolchain the caller resolved (go.mod's, via the caller's cache), so the fresh module
# cache below never downloads one.
PATH=$(go env GOROOT)/bin:$PATH
export PATH
export GOWORK=off GOSUMDB=off GOFLAGS=-mod=mod GOTOOLCHAIN=local
export GOMODCACHE=$work/modcache
# The fixture proxy first, then the user's module cache as a proxy, then the real one.
export GOPROXY="file://$work/proxy,file://$realcache/cache/download,https://proxy.golang.org"

SUBSTRATE=github.com/inference-sim/blis-substrate
SCHEMAS=github.com/inference-sim/blis-schemas

# fixture publishes version ver of module mod into the proxy; its contract package (dir) holds
# the Go source given as the fourth argument.
fixture() {
  local mod=$1 ver=$2 dir=$3 src=$4 src_dir="$work/src/$1@$2"
  mkdir -p "$src_dir/$dir" "$work/proxy/$mod/@v"
  printf 'module %s\n\ngo 1.24\n' "$mod" >"$src_dir/go.mod"
  printf '%s\n' "$src" >"$src_dir/$dir/embed.go"
  cp "$src_dir/go.mod" "$work/proxy/$mod/@v/$ver.mod"
  printf '{"Version":"%s","Time":"2026-01-01T00:00:00Z"}\n' "$ver" >"$work/proxy/$mod/@v/$ver.info"
  (cd "$work/src" && zip -qr "$work/proxy/$mod/@v/$ver.zip" "$mod@$ver")
  echo "$ver" >>"$work/proxy/$mod/@v/list"
}

BASE='package embed

type Router interface {
	Stop()
}
'
ADD_METHOD='package embed

type Router interface {
	Stop()
	Pool() string
}
'
ADD_FUNC='package embed

type Router interface {
	Stop()
}

func Version() string { return "2" }
'
fixture $SUBSTRATE v0.1.0 contracts/embed "$BASE"
fixture $SUBSTRATE v0.2.0 contracts/embed "$ADD_METHOD"
fixture $SUBSTRATE v0.3.0 contracts/embed "$ADD_FUNC"
fixture $SCHEMAS v0.7.0 embed "$BASE"
# v0.9.0 has a go.mod and info but no zip: resolving it fails.
printf 'module %s\n\ngo 1.24\n' "$SUBSTRATE" >"$work/proxy/$SUBSTRATE/@v/v0.9.0.mod"
printf '{"Version":"v0.9.0","Time":"2026-01-01T00:00:00Z"}\n' >"$work/proxy/$SUBSTRATE/@v/v0.9.0.info"

fails=0

# run_case name want(0|1) want_text old_mod old_ver new_mod new_ver [new_major] [script args]
# The script args (word-split) come before the mode.
run_case() {
  local name=$1 want=$2 text=$3 old_mod=$4 old_ver=$5 new_mod=$6 new_ver=$7 major=${8:-} sargs=${9:-}
  local repo=$work/repo-$name out status
  mkdir -p "$repo/hack/hosting-lint"
  cp "$script" "$repo/hack/embed-apidiff.sh"
  (
    cd "$repo"
    git init -q
    git config user.email test@example.invalid
    git config user.name test
    git config commit.gpgsign false
    git commit -q --allow-empty -m "upstream 1"
    git tag v1.0.0
    u1=$(git rev-parse HEAD)
    git commit -q --allow-empty -m "upstream 2"
    git tag v2.0.0
    u2=$(git rev-parse HEAD)
    printf 'upstream_base = %s\n' "$u1" >hack/hosting-lint/budget.txt
    printf 'module example.com/host\n\ngo 1.24\n\nrequire %s %s\n' "$old_mod" "$old_ver" >go.mod
    git add -A
    git commit -q -m "fork release"
    git tag v1.0.0-blis.1
    if [ -n "$major" ]; then printf 'upstream_base = %s\n' "$u2" >hack/hosting-lint/budget.txt; fi
    printf 'module example.com/host\n\ngo 1.24\n\nrequire %s %s\n' "$new_mod" "$new_ver" >go.mod
    git add -A
    git commit -q --allow-empty -m "after the release"
  )
  status=0
  # shellcheck disable=SC2086
  out=$(cd "$repo" && bash hack/embed-apidiff.sh $sargs contract 2>&1) || status=$?
  if [ "$want" = 0 ] && [ "$status" != 0 ] || [ "$want" != 0 ] && [ "$status" = 0 ]; then
    echo "FAIL $name: exit $status, want $([ "$want" = 0 ] && echo 0 || echo non-zero)"
    echo "$out" | sed 's/^/    /'
    fails=$((fails + 1))
  elif ! grep -Fq -- "$text" <<<"$out"; then
    echo "FAIL $name: output lacks \"$text\""
    echo "$out" | sed 's/^/    /'
    fails=$((fails + 1))
  else
    echo "ok   $name (exit $status)"
  fi
}

# 1. Known bad: a method added to the contract's interface breaks every implementation.
run_case incompatible 1 "incompatible changes in embed contract" $SUBSTRATE v0.1.0 $SUBSTRATE v0.2.0
# 2. A compatible addition.
run_case compatible 0 "no incompatible change since v1.0.0-blis.1" $SUBSTRATE v0.1.0 $SUBSTRATE v0.3.0
# 3. The same break across a new upstream major is allowed.
run_case upstream-major 0 "is a new major" $SUBSTRATE v0.1.0 $SUBSTRATE v0.2.0 major
# 4. The contract moved from blis-schemas (package embed) to blis-substrate (contracts/embed).
run_case moved-incompatible 1 "$SCHEMAS v0.7.0 embed -> $SUBSTRATE v0.2.0 contracts/embed" $SCHEMAS v0.7.0 $SUBSTRATE v0.2.0
run_case moved-compatible 0 "$SCHEMAS v0.7.0 embed -> $SUBSTRATE v0.3.0 contracts/embed" $SCHEMAS v0.7.0 $SUBSTRATE v0.3.0
# 5. Control: the same pin compares nothing.
run_case unchanged 0 "no change" $SUBSTRATE v0.1.0 $SUBSTRATE v0.1.0

# 6. Accept-break. The known-bad change passes under a valid review-set id, and is still printed.
run_case ab-valid 0 "ACCEPTED BREAK: the incompatible changes in embed contract" $SUBSTRATE v0.1.0 $SUBSTRATE v0.2.0 "" "--accept-break inference-sim/blis-substrate#42"
run_case ab-printed 0 "Pool" $SUBSTRATE v0.1.0 $SUBSTRATE v0.2.0 "" "--accept-break inference-sim/blis-substrate#42"
run_case ab-names-set 0 "accepted by contract-change review set inference-sim/blis-substrate#42" $SUBSTRATE v0.1.0 $SUBSTRATE v0.2.0 "" "--accept-break inference-sim/blis-substrate#42"
run_case ab-missing 1 "usage: embed-apidiff.sh" $SUBSTRATE v0.1.0 $SUBSTRATE v0.2.0 "" "--accept-break"
run_case ab-malformed 1 "is not the tracking issue" $SUBSTRATE v0.1.0 $SUBSTRATE v0.2.0 "" "--accept-break some-text"
run_case ab-no-number 1 "is not the tracking issue" $SUBSTRATE v0.1.0 $SUBSTRATE v0.2.0 "" "--accept-break a/b#"
# No flag: the known-bad change still fails.
run_case ab-absent 1 "incompatible changes in embed contract" $SUBSTRATE v0.1.0 $SUBSTRATE v0.2.0
# Accept-break does not mask an error that is not an API change.
run_case ab-unresolvable 1 "cannot resolve" $SUBSTRATE v0.1.0 $SUBSTRATE v0.9.0 "" "--accept-break inference-sim/blis-substrate#42"
# The environment variable works the same way, and an empty one is refused.
ACCEPT_BREAK=inference-sim/blis-substrate#42 run_case ab-env 0 "ACCEPTED BREAK" $SUBSTRATE v0.1.0 $SUBSTRATE v0.2.0
ACCEPT_BREAK= run_case ab-env-empty 1 "is not the tracking issue" $SUBSTRATE v0.1.0 $SUBSTRATE v0.2.0

if [ "$fails" != 0 ]; then
  echo "$fails case(s) failed (scratch directory $work)"
  exit 1
fi
echo "all cases passed (scratch directory $work)"
