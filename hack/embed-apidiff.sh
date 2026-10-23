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
#   embed-apidiff.sh fork      pkg/embed against the latest v*-blis* tag reachable from HEAD
#   embed-apidiff.sh contract  blis-schemas' embed package, between the version that tag's go.mod
#                              pins and the version go.mod pins now
#
# Either fails on an incompatible change, unless the upstream release the fork is based on
# (hack/hosting-lint/budget.txt upstream_base, by its vX.Y.Z tag) has another major than the
# tag's X: a new upstream major may change the fork's contract.
#
# The first release under this rule needs no exception: when the tag has no pkg/embed (v1.0.0-blis
# predates it) or pins no blis-schemas, there is no previous surface, every change is an
# addition, and the check passes, saying so.
set -euo pipefail

APIDIFF=golang.org/x/exp/cmd/apidiff@v0.0.0-20260908205506-85c1c2202aba
mode=${1:?usage: embed-apidiff.sh fork|contract}

# Only well-formed fork release tags count: vX.Y.Z-blis (the release before this rule) or
# vX.Y.Z-blis.N. Anything else matching the glob (a stray or experimental tag) is ignored.
prev=$(git -c versionsort.suffix=- tag --merged HEAD --list 'v*-blis*' --sort=-v:refname |
  grep -E '^v[0-9]+\.[0-9]+\.[0-9]+-blis(\.[0-9]+)?$' | head -n1 || true)
if [ -z "$prev" ]; then
  echo "no v*-blis* tag reachable from HEAD: nothing to compare against"
  exit 0
fi

major() { sed -E 's/^v([0-9]+)\..*/\1/' <<<"$1"; }
base=$(sed -n 's/^upstream_base *= *//p' hack/hosting-lint/budget.txt)
upstream=$(git tag --points-at "$base" --list 'v[0-9]*.[0-9]*.[0-9]*' | grep -Ev -- '-' | sort -V | tail -n1 || true)
if [ -z "$upstream" ]; then
  echo "upstream_base $base carries no vX.Y.Z release tag (fetch the upstream tags)" >&2
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

# compare fails on any incompatible change from old to new export data.
compare() {
  report=$(GOWORK=off go run "$APIDIFF" -incompatible "$1" "$2")
  if [ -n "$report" ]; then
    echo "incompatible changes in $3 since $prev:"
    echo "$report"
    exit 1
  fi
  echo "$3: no incompatible change since $prev"
}

case "$mode" in
fork)
  if ! git cat-file -e "$prev:pkg/embed" 2>/dev/null; then
    echo "$prev has no pkg/embed: it is new in this release, so every change is compatible"
    exit 0
  fi
  git worktree add --detach --quiet "$work/prev" "$prev"
  exports "$work/prev" ./pkg/embed "$work/old"
  exports . ./pkg/embed "$work/new"
  compare "$work/old" "$work/new" pkg/embed
  ;;
contract)
  pin() { go mod edit -json "$1" | jq -r '.Require[]? | select(.Path == "github.com/inference-sim/blis-schemas") | .Version'; }
  git show "$prev:go.mod" >"$work/prev.mod"
  old=$(pin "$work/prev.mod")
  new=$(pin go.mod)
  if [ -z "$old" ]; then
    echo "$prev pins no blis-schemas: the embed contract is new in this release, so every change is compatible"
    exit 0
  fi
  if [ "$old" = "$new" ]; then
    echo "blis-schemas $new, as $prev pins: no change"
    exit 0
  fi
  dir() { GOWORK=off GOFLAGS=-mod=mod go mod download -json "github.com/inference-sim/blis-schemas@$1" | jq -r .Dir; }
  exports "$(dir "$old")" ./embed "$work/old"
  exports "$(dir "$new")" ./embed "$work/new"
  compare "$work/old" "$work/new" "blis-schemas embed ($old -> $new)"
  ;;
*)
  echo "usage: embed-apidiff.sh fork|contract" >&2
  exit 2
  ;;
esac
