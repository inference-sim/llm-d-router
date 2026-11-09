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

# embed-apidiff.sh checks that the fork's versioned surface changes only compatibly between fork
# releases on one upstream major (fork tags are vX.Y.Z-blis.N; see pkg/embed):
#
#   embed-apidiff.sh [--accept-break ID] fork
#                              pkg/embed against the latest v*-blis* tag reachable from HEAD
#   embed-apidiff.sh [--accept-break ID] contract
#                              the embed contract, between the version that tag's go.mod pins and
#                              the version go.mod pins now
#
# Either fails on an incompatible change, unless the upstream release the fork is based on
# (hack/hosting-lint/budget.txt upstream_base, by its vX.Y.Z tag) has another major than the
# tag's X: a new upstream major may change the fork's contract.
#
# Accept-break mode (RTR-3, design 12.8). A contract-change review set may make an incompatible
# change to the embed contract. It says so with --accept-break ID, or the ACCEPT_BREAK environment
# variable, where ID is the tracking issue of that review set, written owner/repo#N. The script
# then still runs apidiff and prints every incompatible change, and says that review set accepted
# them; it exits 0 for those changes only. Any other error (an unresolvable module, an apidiff
# failure) still fails. An empty or malformed ID is refused (exit 2), also when ACCEPT_BREAK is set
# but empty, so the mode cannot be left on by accident: every run that uses it names the review
# set in its output. Without the flag or variable nothing changes.
#
# The contract lives in one of two modules: github.com/inference-sim/blis-substrate, package
# contracts/embed, or, up to v1.0.0-blis.2, github.com/inference-sim/blis-schemas, package embed.
# When the tag pins the contract in another module than HEAD does, the contract check compares the
# two packages across modules, and the fork check compares pkg/embed as the tag wrote it, with its
# contract imports and requirement moved to HEAD's module, against pkg/embed now. The contract
# check covers the contract itself, so the fork check reports only changes in pkg/embed.
#
# The first release under this rule needs no exception: when the tag has no pkg/embed (v1.0.0-blis
# predates it) or pins no contract module, there is no previous surface, every change is an
# addition, and the check passes, saying so.
set -euo pipefail

APIDIFF=golang.org/x/exp/cmd/apidiff@v0.0.0-20260908205506-85c1c2202aba
usage="usage: embed-apidiff.sh [--accept-break owner/repo#N] fork|contract"
accept=""
accepting=0
if [ "${ACCEPT_BREAK+set}" = set ]; then
  accepting=1
  accept=$ACCEPT_BREAK
fi
if [ "${1:-}" = --accept-break ]; then
  [ $# -ge 2 ] || {
    echo "--accept-break needs the contract-change review set's tracking issue, owner/repo#N" >&2
    exit 2
  }
  accepting=1
  accept=$2
  shift 2
fi
mode=${1:-}
case "$mode" in fork | contract) ;; *)
  echo "$usage" >&2
  exit 2
  ;;
esac
[ $# -eq 1 ] || {
  echo "$usage" >&2
  exit 2
}
if [ "$accepting" = 1 ] && ! [[ "$accept" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+#[1-9][0-9]*$ ]]; then
  echo "accept-break id \"$accept\" is not the tracking issue of a contract-change review set, owner/repo#N" >&2
  exit 2
fi

# release is the upstream release (its vX.Y.Z tag) that a commit's hack/hosting-lint/budget.txt
# names as upstream_base, or "" when that commit records none.
release() {
  local base
  base=$(git show "$1:hack/hosting-lint/budget.txt" 2>/dev/null | sed -n 's/^upstream_base *= *//p')
  [ -n "$base" ] || return 0
  git tag --points-at "$base" --list 'v[0-9]*.[0-9]*.[0-9]*' | grep -Ev -- '-' | sort -V | tail -n1 || true
}

# The baseline is the newest fork release reachable from HEAD. A fork release is, by definition
# (the fork's tag rule: vX.Y.Z-blis.N, where X.Y.Z is the upstream release merged into llm-d-r),
# a tag that is well formed (vX.Y.Z-blis.N, or vX.Y.Z-blis before the rule) AND whose X.Y.Z is the
# upstream release its own budget.txt records as upstream_base. A tag failing either test (a stray,
# experimental or mistyped tag such as v999.0.0-blis.1 on a v1.0.0 base) is not a release and is
# skipped, so it can neither redirect nor disable the comparison.
prev=""
for t in $(git -c versionsort.suffix=- tag --merged HEAD --list 'v*-blis*' --sort=-v:refname); do
  [[ "$t" =~ ^(v[0-9]+\.[0-9]+\.[0-9]+)-blis(\.[0-9]+)?$ ]] || continue
  if [ "$(release "$t")" = "${BASH_REMATCH[1]}" ]; then
    prev=$t
    break
  fi
  echo "skipping $t: not a fork release (its upstream_base is not ${BASH_REMATCH[1]})" >&2
done
if [ -z "$prev" ]; then
  echo "no fork release tag reachable from HEAD: nothing to compare against"
  exit 0
fi

major() { sed -E 's/^v([0-9]+)\..*/\1/' <<<"$1"; }
upstream=$(release HEAD)
if [ -z "$upstream" ]; then
  echo "upstream_base in hack/hosting-lint/budget.txt carries no vX.Y.Z release tag (fetch the upstream tags)" >&2
  exit 1
fi
if [ "$(major "$upstream")" != "$(major "$prev")" ]; then
  echo "upstream base $upstream is a new major against $prev: incompatible changes are allowed"
  exit 0
fi

work=$(mktemp -d)
trap 'git worktree remove --force "$work/prev" >/dev/null 2>&1 || true; rm -rf "$work"' EXIT

# exports writes the export data of package pkg of the module in dir to file.
exports() {
  (cd "$1" && GOWORK=off GOFLAGS=-mod=mod go run "$APIDIFF" -w "$3" "$2")
}

# compare fails on any incompatible change from old to new export data, unless accept-break names
# the review set that accepted them; the changes are printed either way.
compare() {
  report=$(GOWORK=off go run "$APIDIFF" -incompatible "$1" "$2")
  if [ -n "$report" ]; then
    echo "incompatible changes in $3 since $prev:"
    echo "$report"
    if [ "$accepting" = 1 ]; then
      echo "ACCEPTED BREAK: the incompatible changes in $3 were accepted by contract-change review set $accept"
      return 0
    fi
    exit 1
  fi
  echo "$3: no incompatible change since $prev"
  if [ "$accepting" = 1 ]; then
    echo "note: accept-break $accept was given, but $3 has no incompatible change; drop it"
  fi
}

# contract prints the module, its version and the contract's package directory in the module, for
# the go.mod file given, or nothing when it requires neither contract module.
contract() {
  local mod path ver
  mod=$(go mod edit -json "$1")
  for path in github.com/inference-sim/blis-substrate github.com/inference-sim/blis-schemas; do
    ver=$(jq -r --arg p "$path" '.Require[]? | select(.Path == $p) | .Version' <<<"$mod")
    if [ -n "$ver" ]; then
      case "$path" in
      */blis-substrate) echo "$path $ver ./contracts/embed" ;;
      */blis-schemas) echo "$path $ver ./embed" ;;
      esac
      return 0
    fi
  done
}

git show "$prev:go.mod" >"$work/prev.mod"
read -r old_mod old_ver old_pkg < <(contract "$work/prev.mod" || true) || true
read -r new_mod new_ver new_pkg < <(contract go.mod || true) || true
if [ -z "${new_mod:-}" ]; then
  echo "go.mod requires no embed contract module" >&2
  exit 1
fi

case "$mode" in
fork)
  if ! git cat-file -e "$prev:pkg/embed" 2>/dev/null; then
    echo "$prev has no pkg/embed: it is new in this release, so every change is compatible"
    exit 0
  fi
  git worktree add --detach --quiet "$work/prev" "$prev"
  if [ -n "${old_mod:-}" ] && [ "$old_mod" != "$new_mod" ]; then
    old_imp="$old_mod/${old_pkg#./}"
    new_imp="$new_mod/${new_pkg#./}"
    echo "$prev imports the contract as $old_imp; comparing its pkg/embed with that import moved to $new_imp"
    (
      cd "$work/prev"
      go mod edit -droprequire "$old_mod" -require "$new_mod@$new_ver"
      for f in pkg/embed/*.go; do
        sed -e "s#\"$old_imp/embedtest\"#\"$new_mod/contracts/conformance/embedtest\"#" -e "s#\"$old_imp\"#\"$new_imp\"#" "$f" >"$f.moved"
        mv "$f.moved" "$f"
      done
      if grep -rq "\"$old_mod" pkg/embed; then
        echo "pkg/embed at $prev still names $old_mod after the move" >&2
        exit 1
      fi
    )
  fi
  exports "$work/prev" ./pkg/embed "$work/old"
  exports . ./pkg/embed "$work/new"
  compare "$work/old" "$work/new" pkg/embed
  ;;
contract)
  if [ -z "${old_mod:-}" ]; then
    echo "$prev pins no embed contract module: the contract is new in this release, so every change is compatible"
    exit 0
  fi
  if [ "$old_mod $old_ver" = "$new_mod $new_ver" ]; then
    echo "$new_mod $new_ver, as $prev pins: no change"
    exit 0
  fi
  dir() {
    GOWORK=off GOFLAGS=-mod=mod go mod download -json "$1@$2" | jq -er .Dir ||
      { echo "cannot resolve $1@$2" >&2; return 1; }
  }
  old_dir=$(dir "$old_mod" "$old_ver")
  new_dir=$(dir "$new_mod" "$new_ver")
  exports "$old_dir" "$old_pkg" "$work/old"
  exports "$new_dir" "$new_pkg" "$work/new"
  compare "$work/old" "$work/new" "embed contract ($old_mod $old_ver ${old_pkg#./} -> $new_mod $new_ver ${new_pkg#./})"
  ;;
esac
