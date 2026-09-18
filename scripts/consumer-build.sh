#!/bin/sh
# consumer-build — the CONSUMER-DIRECTION build gate.
#
# WHY THIS EXISTS (do not re-learn this the hard way):
#
# On 2026-08-26 coreutils' main reached a state where `cd bashy && make build`
# FAILED, while BOTH `go test` and `scripts/crossvet.sh` stayed green the whole
# time. cmds/awk had started using interp.Config.DecimalPoint, a symbol that
# exists only in a NEWER goawk fork than the one bashy — a downstream consumer —
# pinned. Nothing caught it because every existing gate compiles coreutils
# against COREUTILS' OWN module resolution. A green coreutils does not mean a
# buildable consumer: MVS + the consumer's own `replace`/pin can select a
# different version of a shared dependency than coreutils resolves for itself,
# and the break only surfaces when the CONSUMER compiles coreutils' packages.
# This gate closes that blind spot by actually building the sibling consumer
# against THIS working tree.
#
# It got worse on inspection, which is the second half of this gate. coreutils
# pinned goawk (via `replace`) to commit 88712e61a085 on branch
# `coreutils-locale-numeric` — a TWO-COMMIT ORPHAN whose root differs from the
# fork's own root, so `git merge-base` finds NO common ancestor with the fork's
# default branch (master). A `replace` aimed at a commit that is not reachable
# from the fork's default branch is one branch-deletion away from making the
# repo permanently unbuildable, and — because such a commit is off the mainline
# — is exactly how a downstream consumer ends up unable to resolve the code it
# needs. So part (b) REFUSES any versioned `replace` whose commit is not an
# ancestor of the target fork's default branch, and names the branch the commit
# actually lives on.
#
# TWO CHECKS, run together:
#   (a) build ../bashy (the flat sibling consumer) against THIS tree; fail
#       loudly with the offending package + symbol when it cannot compile.
#       Skip cleanly (exit 0) when the sibling is absent, so a standalone
#       coreutils clone is not broken by the gate.
#   (b) refuse a module `replace` pinned to a commit that is not an ancestor of
#       the target fork's default branch; report the branch it lives on.
#
# Wired into DAG.md as `consumer-build`, beside `test` and `crossvet`. Run it
# before merge. Network is used only by part (b) to ask the fork about its
# branches; when the fork is unreachable (offline) part (b) WARNS and skips
# rather than failing, so an offline build is never blocked by it.
#
# Usage: scripts/consumer-build.sh [consumer-dir]
#        CONSUMER_DIR=/path/to/bashy scripts/consumer-build.sh
set -eu

cd "$(git rev-parse --show-toplevel)"
root=$(pwd -P)
fail=0

# ---------------------------------------------------------------------------
# Allowlist for part (b): consciously-accepted non-ancestor pins.
#
# Each entry is a `modulepath@<12-hex-commit>` that we KNOW is not an ancestor
# of its fork's default branch and are accepting on purpose, with the reason and
# the real fix. New, unlisted non-ancestor pins hard-fail — that is the check
# that would have caught the orphan before it landed; this list is the visible,
# reviewed record of the debt we are carrying meanwhile.
#
#   github.com/qiangli/goawk@88712e61a085
#     Branch `coreutils-locale-numeric`. Carries the error-bearing regex backend
#     AND locale DecimalPoint API that cmds/awk compiles against; neither is on
#     the fork's `master`, so no ancestor of the default branch can replace it.
#     REAL FIX: land those commits on the fork's default branch (merge
#     `coreutils-locale-numeric` into master, or make it the default), then
#     repin to the resulting mainline commit and delete this entry. Until then
#     the pin is a branch-deletion away from an unbuildable tree — tracked, not
#     hidden.
is_allowlisted() {
	case "$1" in
	github.com/qiangli/goawk@88712e61a085) return 0 ;;
	esac
	return 1
}

# ---------------------------------------------------------------------------
# Part (b): replace-ancestry check.
echo "consumer-build: (b) replace-ancestry check"

# Emit "modulepath<TAB>version" for each `replace` with a remote version target
# (filesystem replaces — ../sh, ./external/... — have an empty New.Version and
# are skipped).
replaces=$(go mod edit -json | python3 -c '
import json, sys
for r in (json.load(sys.stdin).get("Replace") or []):
    new = r.get("New", {})
    ver = new.get("Version", "")
    if ver:
        print(new["Path"] + "\t" + ver)
')

# Extract the 12-hex commit from a Go pseudo-version (…-<14-digit-ts>-<12hex>).
# Plain tags (v1.2.3) print nothing and are skipped: they are durable named
# refs, not raw commit pins.
pseudo_commit() {
	printf '%s\n' "$1" | sed -n 's/.*[0-9]\{14\}-\([0-9a-f]\{12\}\)$/\1/p'
}

# Map a module path to its https git URL (strip any trailing /vN major suffix).
module_url() {
	printf 'https://%s\n' "$(printf '%s\n' "$1" | sed 's#/v[0-9]\{1,\}$##')"
}

if [ -z "$replaces" ]; then
	echo "consumer-build:   no versioned replace directives — nothing to check"
else
	# Iterate without a subshell so `fail` survives.
	oldIFS=$IFS
	IFS='
'
	for line in $replaces; do
		path=${line%%	*}
		ver=${line#*	}
		commit=$(pseudo_commit "$ver")
		if [ -z "$commit" ]; then
			echo "consumer-build:   $path $ver — tag pin, skipped (not a raw commit)"
			continue
		fi
		url=$(module_url "$path")

		# Ask the fork what its default branch and branch tips are (cheap,
		# ref-only). Two calls: a positional ref pattern (HEAD) and --heads
		# cannot be combined — git filters to the pattern and drops the heads.
		# Any failure here is treated as "cannot verify" → warn.
		symref=$(git ls-remote --symref "$url" HEAD 2>/dev/null || true)
		allheads=$(git ls-remote --heads "$url" 2>/dev/null || true)
		if [ -z "$symref" ] || [ -z "$allheads" ]; then
			echo "consumer-build:   WARN $path@$commit — could not reach $url (offline?); ancestry NOT verified"
			continue
		fi
		def=$(printf '%s\n' "$symref" | sed -n 's#^ref: refs/heads/\([^	]*\)	HEAD#\1#p')
		if [ -z "$def" ]; then
			echo "consumer-build:   WARN $path@$commit — $url advertised no default branch; ancestry NOT verified"
			continue
		fi
		# Branch whose tip == this commit (names the orphan for the report).
		tipbranch=$(printf '%s\n' "$allheads" | sed -n "s#^${commit}[0-9a-f]*	refs/heads/\\(.*\\)#\\1#p" | head -n1)
		deftip=$(printf '%s\n' "$allheads" | sed -n "s#	refs/heads/${def}\$##p" | head -n1)

		verdict=""
		if [ -n "$deftip" ] && printf '%s\n' "$deftip" | grep -qi "^${commit}"; then
			verdict=ancestor # commit is the default-branch tip
		else
			# Interior/other: fetch default-branch history (commits only) and
			# test for the object's presence == reachable from default == ancestor.
			tmp=$(mktemp -d)
			if git -C "$tmp" init -q &&
				git -C "$tmp" fetch -q --filter=blob:none --no-tags "$url" "$def" 2>/dev/null &&
				git -C "$tmp" cat-file -e "${commit}^{commit}" 2>/dev/null; then
				verdict=ancestor
			elif [ -d "$tmp/.git" ] || [ -f "$tmp/HEAD" ]; then
				# Fetch succeeded but object absent → not reachable from default.
				# (If the fetch itself failed we cannot tell — treat as unverified.)
				if git -C "$tmp" rev-parse --verify -q "$def" >/dev/null 2>&1 ||
					git -C "$tmp" rev-parse --verify -q FETCH_HEAD >/dev/null 2>&1; then
					verdict=orphan
				else
					verdict=unverified
				fi
			else
				verdict=unverified
			fi
			rm -rf "$tmp"
		fi

		case "$verdict" in
		ancestor)
			echo "consumer-build:   OK   $path@$commit is an ancestor of $def"
			;;
		unverified)
			echo "consumer-build:   WARN $path@$commit — could not fetch $def from $url; ancestry NOT verified"
			;;
		orphan)
			where=${tipbranch:-"no branch tip (dangling or interior of a non-default branch)"}
			if is_allowlisted "$path@$commit"; then
				echo "consumer-build:   WARN $path@$commit is NOT an ancestor of $def (lives on: $where) — ALLOWLISTED, accepted debt; see scripts/consumer-build.sh"
			else
				echo "consumer-build:   FAIL $path@$commit is NOT an ancestor of the fork's default branch ($def)."
				echo "consumer-build:        The commit lives on: $where."
				echo "consumer-build:        A replace pinned off the default branch is one branch-deletion"
				echo "consumer-build:        away from an unbuildable tree, and off-mainline code a consumer"
				echo "consumer-build:        cannot resolve. Land it on $def and repin, or allowlist it in"
				echo "consumer-build:        scripts/consumer-build.sh with a reason."
				fail=1
			fi
			;;
		esac
	done
	IFS=$oldIFS
fi

# ---------------------------------------------------------------------------
# Part (a): build the flat sibling consumer against THIS working tree.
echo "consumer-build: (a) sibling consumer build"

consumer=${1:-${CONSUMER_DIR:-"$(dirname "$root")/bashy"}}
if [ ! -f "$consumer/go.mod" ]; then
	echo "consumer-build:   sibling consumer not found at $consumer — skipping (standalone clone). Set CONSUMER_DIR or pass a path to run it."
else
	consumer=$(cd "$consumer" && pwd -P)
	echo "consumer-build:   building $consumer against $root"

	# Redirect the consumer's coreutils replace at THIS tree, build, then always
	# restore its go.mod/go.sum — the gate must leave the consumer untouched.
	backup=$(mktemp -d)
	cp "$consumer/go.mod" "$backup/go.mod"
	[ -f "$consumer/go.sum" ] && cp "$consumer/go.sum" "$backup/go.sum"
	restore() {
		cp "$backup/go.mod" "$consumer/go.mod"
		[ -f "$backup/go.sum" ] && cp "$backup/go.sum" "$consumer/go.sum"
		rm -rf "$backup"
	}
	trap restore EXIT INT TERM

	# `if (...)` keeps `set -e` from killing the script when the consumer fails
	# to compile — that failure is the whole point and must be reported, not fatal.
	if (
		cd "$consumer"
		# GOWORK=off so a stray go.work cannot silently override our replace;
		# -mod=mod lets go add any go.sum entries the redirected graph needs.
		GOWORK=off go mod edit -replace "github.com/qiangli/yoke=$root"
		GOWORK=off go build -mod=mod ./...
	); then
		build_rc=0
	else
		build_rc=1
	fi

	restore
	trap - EXIT INT TERM

	if [ "$build_rc" -ne 0 ]; then
		echo "consumer-build:   FAIL consumer $consumer does not compile against this tree (see error above)."
		fail=1
	else
		echo "consumer-build:   OK   consumer compiles against this tree"
	fi
fi

# ---------------------------------------------------------------------------
if [ "$fail" -ne 0 ]; then
	echo "consumer-build: FAIL"
	exit 1
fi
echo "consumer-build: PASS"
