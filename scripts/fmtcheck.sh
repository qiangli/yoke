#!/bin/sh
# fmtcheck — the formatting gate. REPORTS, NEVER REWRITES.
#
# THE ONE implementation, same shape as scripts/crossvet.sh: every caller
# delegates here so the scope can never drift between them.
#   - .github/workflows/test.yml (the ubuntu leg)
#   - DAG.md's `fmtcheck` task (manual: `bashy dag fmtcheck`)
#
# # Why this fails instead of fixing
#
# The obvious version of this gate runs `gofmt -w` and commits the result. Do
# not write that version. gofmt does not only move whitespace: on doc comments
# it applies godoc's legacy typographic substitution, turning a pair of ASCII
# single-quotes into a curly closing quote.
#
# That is a silent content change, and it has already bitten this tree. A
# comment in bashy's bash53suite cited the exact regression its baseline once
# carried — a trap listing whose argument is an empty string, written with that
# very pair of quotes. Auto-formatting rewrote it into a curly quote, leaving a
# signal-fidelity harness documenting shell syntax that does not exist. Nothing
# would have reported it: the diff looks like formatting, and formatting diffs
# are what reviewers skim.
#
# So the rule is: a human decides. When a file legitimately cannot take gofmt's
# output, the fix is to restructure it (move the literal into an indented
# doc-comment code block, where the characters survive verbatim) — not to accept
# a rewrite of what the code says.
#
# # Scope
#
# Tracked .go files only, so build output and scratch files never fail the gate.
#
# The only exclusions are the two VENDORED SUBMODULES (external/ollama/src,
# external/podman/src) — upstream code, upstream formatting. Everything else
# under external/ is ours (the binmgr wrappers and toolchain provisioners) and is
# gated like any other package. A blanket ^external/ skip was letting our own
# code through unchecked.
#
# This scope is deliberately WIDER than the test scope, which skips all of
# external/ because those packages pull cgo and platform backends. Formatting is
# not compilation: gofmt parses, it does not build, so there is no reason for our
# own code to escape the gate just because running it needs a Linux box.
set -eu

root="$(cd "$(dirname "$0")/.." && pwd -P)"
cd "$root"

files="$(git ls-files '*.go' | grep -Ev '^external/(ollama|podman)/src/' || true)"
if [ -z "$files" ]; then
	echo "fmtcheck: no tracked Go files"
	exit 0
fi

# shellcheck disable=SC2086
unformatted="$(gofmt -l $files)"
if [ -z "$unformatted" ]; then
	echo "fmtcheck: PASS ($(echo "$files" | wc -l | tr -d ' ') files)"
	exit 0
fi

echo "fmtcheck: FAIL — these files are not gofmt-clean:" >&2
echo "$unformatted" | sed 's/^/  /' >&2
echo >&2
echo "diff:" >&2
# shellcheck disable=SC2086
gofmt -d $unformatted >&2
echo >&2
echo "Fix with: gofmt -w <file>" >&2
echo >&2
echo "READ THE DIFF FIRST. gofmt rewrites doc-comment text as well as" >&2
echo "whitespace — notably a pair of ASCII single-quotes becomes a curly" >&2
echo "closing quote. If a file quotes shell or Go syntax in a comment, move" >&2
echo "the literal into an indented code block rather than accepting the" >&2
echo "rewrite; see the header of this script." >&2
exit 1
