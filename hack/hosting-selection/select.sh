#!/usr/bin/env bash
# Prints "hosted=true" when a change touches what the plugin-audit and conformance jobs of
# hosting-lint.yml test, and "hosted=false" otherwise. It reads the changed paths, one a line,
# from standard input. An empty list means the change is unknown, so the answer is true.
#
# What those jobs test is pkg/embed against the hosting contract (blisembed/) and the router's
# hosted packages: the packages under pkg/ and cmd/, the reference endpoint-picker configs, the checks and tools under hack/, the
# module files and the workflow itself.
set -euo pipefail
hosted='^(blisembed/|pkg/|cmd/|reference-configs/|hack/|go\.mod$|go\.sum$|\.github/workflows/hosting-lint\.yml$)'
n=0; yes=false
while IFS= read -r path; do
	[ -n "$path" ] || continue
	n=$((n + 1))
	if [[ "$path" =~ $hosted ]]; then yes=true; fi
done
if [ "$n" -eq 0 ]; then yes=true; fi
echo "hosted=$yes"
