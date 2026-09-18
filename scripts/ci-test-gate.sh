#!/usr/bin/env bash
# ci-test-gate.sh — the no-regression ratchet for the Go test suite.
#
# WHY THIS EXISTS. Tests for OPEN stories are committed to main deliberately
# red (the mv story's contract literally says "prove the behavioral tests red on
# the current base"). A plain `go test` gate is therefore permanently red, which
# means it reports nothing: a genuine new break is indistinguishable from the
# known ones, and everybody learns to ignore the job.
#
# This is bashy's scripts/ci-bash53-gate.sh applied to Go tests:
#
#   * a test that fails but is NOT in the baseline -> re-run once, alone. Fails
#     again -> NEW regression -> fail. Passes -> reported FLAKY, not fatal.
#   * a baseline test that now passes -> progress -> fail, demanding you delete
#     its line, so the baseline only shrinks and never silently drifts stale.
#   * baseline == actual -> pass.
#
# The re-run is not tolerance-by-silence. It is positive evidence — the test was
# executed a second time and passed — both outcomes are printed, and a test that
# fails twice is a regression with no appeal. Without it a ~6800-test suite on a
# shared runner is a coin flip, and a gate that is a coin flip gets muted, which
# is exactly the state this script was written to end.
#
# When the baseline reaches empty, replace this with a plain `go test`.
#
# TWO RULES THAT ARE NOT NEGOTIABLE, both learned the expensive way here:
#
# 1. A package that fails WITHOUT a failing test — a build error, a setup
#    panic — is ALWAYS a hard failure and can never be baselined. coreutils CI
#    spent weeks failing at exactly that: pkg/webconsole gained a
#    ../filebrowser flat-sibling replace the workflow never cloned, so the job
#    died before running one test and the entire suite went unmeasured. A gate
#    that lets a build error look like a known failure would re-create that.
#
# 2. An INCONCLUSIVE run is never scored. Zero passing tests means the suite did
#    not run, and every baseline entry would look "fixed" — emitting a bogus
#    progress report and a green build off a suite that never executed.
set -uo pipefail

cd "$(dirname "$0")/.."
BASELINE=test/known-failures.txt
GOOS=$(go env GOOS)

[ -f "$BASELINE" ] || { echo "gate: missing $BASELINE" >&2; exit 2; }

# The package set the workflow tests: coreutils' own packages, minus the
# vendored external/ forks (cgo + platform backends, upstream's to test), plus
# the lean pure-Go externals so they stay built on every platform.
base="$(go list ./... | grep -v '/external/')"
if [ "$GOOS" = "windows" ]; then
    base="$(printf '%s\n' "$base" | grep -vE '/pkg/weave$')"
fi
pkgs="$base
github.com/qiangli/yoke/external/gotoolchain
github.com/qiangli/yoke/external/act
github.com/qiangli/yoke/external/gh"

events=$(mktemp)
trap 'rm -f "$events" "$events".*' EXIT
# -json so a test name is attributed to its package even when packages run in
# parallel. Failure is expected here; the ratchet below decides the verdict.
# shellcheck disable=SC2086
go test -json $pkgs >"$events" 2>&1
gotest_rc=$?

# One line per event: extract Action/Package/Test without a jq dependency.
# Package import paths and Go test names cannot contain a double quote, so the
# non-greedy field match is exact for these three fields.
extract() { # extract <action> <want-test: yes|no>
    awk -v want_action="$1" -v want_test="$2" '
    {
        action = ""; pkg = ""; test = ""
        if (match($0, /"Action":"[^"]*"/))  { action = substr($0, RSTART+10, RLENGTH-11) }
        if (match($0, /"Package":"[^"]*"/)) { pkg    = substr($0, RSTART+11, RLENGTH-12) }
        if (match($0, /"Test":"[^"]*"/))    { test   = substr($0, RSTART+8,  RLENGTH-9)  }
        if (action != want_action) next
        if (want_test == "yes" && test == "") next
        if (want_test == "no"  && test != "") next
        # Subtests report as Parent/Child; the baseline names top-level tests.
        if (test != "" && index(test, "/") > 0) next
        if (want_test == "yes") { print pkg "\t" test } else { print pkg }
    }' "$events"
}

# Print what a failing test actually said. The gate captures -json into a temp
# file (so a test name is attributed to its package under parallel execution),
# which means NOTHING reaches the log unless we put it there — the first
# version of this script reported "X failed" and no reason, forcing a re-run to
# learn anything.
show_test_output() { # show_test_output <package> <test>
    awk -v want_pkg="$1" -v want_test="$2" '
    {
        pkg = ""; test = ""; out = ""
        if (match($0, /"Package":"[^"]*"/)) { pkg  = substr($0, RSTART+11, RLENGTH-12) }
        if (match($0, /"Test":"[^"]*"/))    { test = substr($0, RSTART+8,  RLENGTH-9)  }
        if (pkg != want_pkg) next
        if (test != want_test && index(test, want_test "/") != 1) next
        if (!match($0, /"Output":".*"\}$/)) next
        out = substr($0, RSTART+10, RLENGTH-12)
        gsub(/\\n$/, "", out); gsub(/\\t/, "\t", out); gsub(/\\"/, "\"", out); gsub(/\\\\/, "\\", out)
        print "    " out
    }' "$events" | head -25
}

passed=$(extract pass yes | sort -u)
failed=$(extract fail yes | sort -u)
failed_pkgs=$(extract fail no | sort -u)

if [ -z "$passed" ]; then
    echo "gate: INCONCLUSIVE — no test passed, so the suite did not run. Refusing to score." >&2
    sed -n '1,40p' "$events" >&2
    exit 2
fi

# A failing package with no failing test of its own is a build/setup failure.
# Not baselineable, ever.
broken=""
while IFS= read -r pkg; do
    [ -n "$pkg" ] || continue
    if ! printf '%s\n' "$failed" | grep -qF "$pkg	"; then
        broken="$broken $pkg"
    fi
done <<EOF
$failed_pkgs
EOF
if [ -n "$broken" ]; then
    echo "gate: package(s) failed WITHOUT a failing test — a build or setup error, which is never baselineable:" >&2
    for pkg in $broken; do
        echo "  $pkg" >&2
        grep -F "\"Package\":\"$pkg\"" "$events" | grep -F '"Action":"output"' |
            sed -E 's/.*"Output":"//; s/"\}$//; s/\\n$//; s/\\t/\t/g; s/\\"/"/g' | tail -8 >&2
    done
    exit 1
fi

# Baseline for THIS platform. These failures are not portable — mv and pax pass
# on darwin and fail on linux — so an entry applies only to the goos it names.
strict=$(awk -v goos="$GOOS" '
    /^[[:space:]]*(#|$)/ { next }
    $1 == goos && $4 != "sometimes" { print $2 "\t" $3 }' "$BASELINE" | sort -u)
sometimes=$(awk -v goos="$GOOS" '
    /^[[:space:]]*(#|$)/ { next }
    $1 == goos && $4 == "sometimes" { print $2 "\t" $3 }' "$BASELINE" | sort -u)
baseline=$(printf '%s\n%s\n' "$strict" "$sometimes" | grep -v '^$' | sort -u)

new=$(comm -23 <(printf '%s\n' "$failed" | grep -v '^$') <(printf '%s\n' "$baseline" | grep -v '^$'))
fixed=$(comm -13 <(printf '%s\n' "$failed" | grep -v '^$') <(printf '%s\n' "$strict" | grep -v '^$'))

echo "gate: $GOOS — $(printf '%s\n' "$passed" | grep -c .) passed, $(printf '%s\n' "$failed" | grep -c .) failed, $(printf '%s\n' "$baseline" | grep -c .) known"

# An intermittent entry that goes quiet is a success reached by the absence of
# evidence. Say what it did, every run, whichever way it went.
if [ -n "$sometimes" ]; then
    while IFS= read -r entry; do
        [ -n "$entry" ] || continue
        if printf '%s\n' "$failed" | grep -qxF "$entry"; then
            echo "gate: intermittent — FAILED this run: $entry"
        else
            echo "gate: intermittent — passed this run: $entry"
        fi
    done <<EOF
$sometimes
EOF
fi

rc=0
if [ -n "$new" ]; then
    # A suite this size on a shared runner has a flake tail, and a gate that is
    # a coin flip gets muted — which would put us back where we started. So an
    # unbaselined failure is RE-RUN ONCE, alone, and classified by what the
    # second run says. This is not tolerance-by-silence: the retry is positive
    # evidence (the test was executed again and passed), both outcomes are
    # printed, and a test that fails twice is a regression, full stop.
    echo "gate: $(printf '%s\n' "$new" | grep -c .) unbaselined failure(s); output follows, then one confirming re-run each." >&2
    while IFS=$'\t' read -r pkg test; do
        [ -n "$pkg" ] || continue
        echo "gate: --- $pkg $test" >&2
        show_test_output "$pkg" "$test" >&2
    done <<EOF
$new
EOF

    confirmed=""
    flaky=""
    while IFS=$'\t' read -r pkg test; do
        [ -n "$pkg" ] || continue
        if go test -count=1 -run "^${test}\$" "$pkg" >/dev/null 2>&1; then
            flaky="$flaky$pkg\t$test\n"
        else
            confirmed="$confirmed$pkg\t$test\n"
        fi
    done <<EOF
$new
EOF

    if [ -n "$flaky" ]; then
        echo "gate: FLAKY — failed in the suite, PASSED on an isolated re-run:" >&2
        printf "  $flaky" >&2
        echo "gate: not treated as a regression, but it is real. File a story; a test that only sometimes holds is not evidence." >&2
    fi
    if [ -n "$confirmed" ]; then
        echo "gate: NEW regression(s) — failed twice, and NOT in $BASELINE:" >&2
        printf "  $confirmed" >&2
        echo "gate: fix the regression, or (only if intended, and only with a story) add it to the baseline with its cause." >&2
        rc=1
    fi
fi

if [ -n "$fixed" ]; then
    echo "gate: PROGRESS — these baseline tests now PASS; delete their lines from $BASELINE:" >&2
    printf '  %s\n' "$fixed" >&2
    rc=1
fi

if [ "$rc" -eq 0 ]; then
    if [ "$gotest_rc" -ne 0 ] && [ -z "$failed" ]; then
        echo "gate: go test exited $gotest_rc but reported no failing test — refusing to pass on an unexplained failure." >&2
        exit 2
    fi
    echo "gate: OK — actual failure set matches the baseline for $GOOS."
fi
exit $rc
