#!/bin/sh
# crossvet — the cross-OS compile gate for the CI scope.
#
# THE ONE implementation. Both callers delegate here so the target list can
# never drift between them:
#   - scripts/hooks/pre-push  (automatic, once `git config core.hooksPath scripts/hooks`)
#   - DAG.md's `crossvet` task (manual: `bashy dag crossvet`)
#
# Why this exists: `go test` on darwin STRUCTURALLY cannot see a build-tag
# break — a file that is //go:build !windows never compiles for windows, so a
# darwin-green suite proves nothing about the leg that ships. `go vet`
# compiles every package INCLUDING tests for the target GOOS, which is exactly
# the class of break the CI windows leg keeps catching after darwin-only local
# work (unix-only types like syscall.Stat_t in an untagged _test.go).
#
# (The js/wasip1 chmod/chgrp/chown and mv canaries are coreutils'.)
#
# The aix build is a DELIBERATE canary, not a shipping target. A build tag that
# says `!windows` is a claim that every other OS is a unix with flock — and aix
# and solaris lock through fcntl, so such a tag does not merely mislabel them,
# it fails to COMPILE. Locking code is where this keeps happening (pkg/steward,
# pkg/policy/coord), and the fail-closed implementations those packages ship
# for unsupported platforms are only reachable if the package builds there at
# all. `go build` rather than `go vet`, since aix has no test-runner story and
# the point is the tag selection.
#
# Scope EXCLUDES the vendored external/ forks (ollama, podman): they pull cgo +
# platform backends and are upstream's to test. That is the CI scope.
#
# Usage: scripts/crossvet.sh            # windows linux darwin + wasm/aix canaries
#        scripts/crossvet.sh windows    # a subset, for a fast inner loop
set -e
cd "$(git rev-parse --show-toplevel)"

scripts/applet-test-coverage.sh

targets=${*:-"windows linux darwin"}
pkgs=$(go list ./... | grep -v /external/)
failed=""

for os in $targets; do
  if GOOS=$os go vet $pkgs; then
    echo "crossvet: GOOS=$os PASS"
  else
    echo "crossvet: GOOS=$os FAIL"
    failed="$failed $os"
  fi
done

# The canaries ride with the full run only, not with an explicit subset.
if [ $# -eq 0 ]; then

  if GOOS=aix GOARCH=ppc64 go build ./pkg/steward/ ./pkg/policy/coord/; then
    echo "crossvet: GOOS=aix PASS (fail-closed-lock canaries)"
  else
    echo "crossvet: GOOS=aix FAIL (fail-closed-lock canaries)"
    failed="$failed aix"
  fi

fi

if [ -n "$failed" ]; then
  echo "crossvet: FAIL —$failed"
  exit 1
fi
echo "crossvet: PASS ($targets$([ $# -eq 0 ] && echo ' aix'))"
