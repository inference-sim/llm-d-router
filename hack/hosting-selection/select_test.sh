#!/usr/bin/env bash
# Sample change lists against select.sh: each must select what the plugin-audit and conformance
# jobs need, so a change that they test never skips them and a docs-only change does.
set -uo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
fail=0
check() { # name want paths...
	local name="$1" want="$2"; shift 2
	local got
	got="$(printf '%s\n' "$@" | "$here/select.sh")"
	if [ "$got" = "hosted=$want" ]; then echo "ok   $name"; else echo "FAIL $name: got $got, want hosted=$want"; fail=1; fi
}
check "a hosted package"            true  pkg/embed/support.go
check "an upstream-owned package"   true  pkg/epp/framework/plugins/scorer/foo.go
check "the reference configs"       true  reference-configs/endpoint-picker/a.yaml
check "the hosting contract"        true  blisembed/embedtest/runtime.go
check "the command"                 true  cmd/epp/main.go
check "a check under hack"          true  hack/hosting-lint/baseline.txt
check "the module files"            true  go.sum
check "this workflow"               true  .github/workflows/hosting-lint.yml
check "docs only"                   false docs/readme.md README.md
check "deployment manifests only"   false deploy/charts/x/values.yaml
check "another workflow only"       false .github/workflows/stale.yaml
check "docs beside a hosted file"   true  docs/readme.md pkg/embed/a.go
check "an unknown change"           true
[ "$(printf '' | "$here/select.sh")" = "hosted=true" ] || { echo "FAIL: an empty list must select"; fail=1; }
exit "$fail"
