#!/usr/bin/env bash
# comms-cli-audit.sh — prove the communication surface is REACHABLE on a real,
# built bashy binary, and spot-check the contracts that package tests cannot
# see from inside the process.
#
# WHY. S80 shipped six comms changes; one (the inbox/notify front doors)
# passed every package test and was still unreachable from the CLI — the
# constructors existed, the host never mounted them. The Go-side audit
# (test/commscli) proves each front door behaves from a built binary; THIS
# script proves the actual bashy binary mounts them. Run it against every
# freshly built bashy. This also pins the non-interactive discovery surface for
# the local POSIX applets, the board aliases, ping, and meet; it never opens an
# interactive terminal conversation:
#
#   BASHY_BIN=/path/to/bashy scripts/comms-cli-audit.sh   # default: bashy on PATH
#
# HERMETIC. Every store is pointed at a fresh temp dir (BASHY_MB_DIR,
# BASHY_ROOM_DIR, BASHY_FLEET_DIR, HOME) and ambient identity variables are
# unset, so it never reads or writes the operator's real board and passes the
# same way on any machine.
#
# Exit 0 = every verb reachable and every checked contract held.
# Non-zero = the failures were printed; each is a real defect in the binary.

set -u

BIN="${BASHY_BIN:-bashy}"

if ! command -v "$BIN" >/dev/null 2>&1; then
  echo "comms-cli-audit: no bashy binary: $BIN (set BASHY_BIN)" >&2
  exit 2
fi

# --- the hermetic host ------------------------------------------------------
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
export HOME="$TMP/home"
export BASHY_MB_DIR="$TMP/mb"
export BASHY_ROOM_DIR="$TMP/room"
export BASHY_FLEET_DIR="$TMP/fleet"
mkdir -p "$HOME" "$BASHY_MB_DIR" "$BASHY_ROOM_DIR" "$BASHY_FLEET_DIR"
# Ambient identity must not leak into what the verbs resolve.
unset BASHY_PRINCIPAL BASHY_AGENT_ID BASHY_AGENT WEAVE_AGENT BASHY_EPISODE WEAVE_EPISODE BASHY_AGENTIC 2>/dev/null || true

fails=0
pass() { printf 'PASS  %s\n' "$1"; }
fail() { printf 'FAIL  %s\n' "$1"; fails=$((fails + 1)); }

# --- 1. REACHABILITY: every registered communication surface must be mounted -
# The exact failure mode this script exists for: a verb whose --help does not
# exit 0 is not on the binary, whatever the package tests say. inbox and
# notify were the verbs found missing; the gap is closed and they are pinned
# here so it cannot silently reopen.
REQUIRED_VERBS=(
	"mail --help"
	"mailx --help"
	"talk --help"
	"write --help"
	"mesg --help"
	"whois --help"
	"agent whoami --help"
	"mb --help"
	"messages --help"
	"mb send --help"
	"ping --help"
	"bus watch --help"
	"meet --help"
	"weave fleet --help"
	"inbox --help"
	"notify --help"
)
for v in "${REQUIRED_VERBS[@]}"; do
  # shellcheck disable=SC2086
  if "$BIN" $v >/dev/null 2>&1; then
    pass "reachable: $BIN $v"
  else
    fail "UNREACHABLE: '$BIN $v' did not exit 0 — the verb is not mounted on this binary"
  fi
done

# Alias identity has two intentional contracts. POSIX mail owns its spelling in
# help and diagnostics even though it shares mailx's implementation. messages is
# a transparent Cobra alias and therefore renders the canonical mb help.
mail_help="$("$BIN" mail --help 2>/dev/null)"
mailx_help="$("$BIN" mailx --help 2>/dev/null)"
if printf '%s\n' "$mail_help" | grep -q '^Usage: mail ' &&
   printf '%s\n' "$mailx_help" | grep -q '^Usage: mailx '; then
	pass "mail/mailx help owns each invoked spelling"
else
	fail "mail/mailx help identity drift: mail='$(printf '%s' "$mail_help" | head -n 1)' mailx='$(printf '%s' "$mailx_help" | head -n 1)'"
fi

mb_help="$("$BIN" mb --help 2>/dev/null)"
messages_help="$("$BIN" messages --help 2>/dev/null)"
if [ "$mb_help" = "$messages_help" ]; then
	pass "messages is a transparent alias of mb"
else
	fail "messages help differs from canonical mb help"
fi

# --- 2. BEHAVIOR spot checks (skipped per-verb if unreachable) --------------

# whois: an observed name resolves with source=observed, never 'names nothing'.
mkdir -p "$BASHY_MB_DIR/seen"
printf '1\n' >"$BASHY_MB_DIR/seen/zz-audit-observed"
out="$("$BIN" whois zz-audit-observed 2>&1)"
if [ $? -eq 0 ] && printf '%s' "$out" | grep -q "observed"; then
  pass "whois resolves an observed name with source=observed"
else
  fail "whois on an observed name: expected exit 0 + source=observed, got: $out"
fi

# mb send to an unresolvable target: non-zero, says failed, writes nothing.
if "$BIN" mb send --as audit-sender zz-no-such-target hello >/dev/null 2>"$TMP/send.err"; then
  fail "mb send to an unresolvable target exited 0"
else
  if grep -q "failed" "$TMP/send.err" && [ ! -s "$BASHY_MB_DIR/posts.jsonl" ]; then
    pass "mb send to an unresolvable target fails and writes nothing"
  else
    fail "mb send failure contract: stderr was '$(cat "$TMP/send.err")', posts.jsonl $(wc -c <"$BASHY_MB_DIR/posts.jsonl" 2>/dev/null || echo 0) bytes"
  fi
fi

# mb --wait timeout: exit 0, EMPTY stdout, note on stderr.
out="$("$BIN" mb --as audit-quiet --wait 300ms 2>"$TMP/wait.err")"
if [ $? -eq 0 ] && [ -z "$out" ] && grep -q "nothing new" "$TMP/wait.err"; then
  pass "mb --wait timeout is an empty successful read"
else
  fail "mb --wait timeout contract: exit $?, stdout '$out', stderr '$(cat "$TMP/wait.err")'"
fi

# bus watch --drain --wait: the same bounded-wait contract.
out="$("$BIN" bus watch --drain --wait 300ms --to audit-nobody --as audit-nobody 2>"$TMP/bus.err")"
if [ $? -eq 0 ] && [ -z "$out" ] && grep -q "nothing new" "$TMP/bus.err"; then
  pass "bus watch --drain --wait timeout is an empty successful read"
else
  fail "bus watch --drain --wait contract: exit $?, stdout '$out', stderr '$(cat "$TMP/bus.err")'"
fi

# agent whoami: identity or a clear refusal — never silently something else.
out="$("$BIN" agent whoami 2>"$TMP/who.err")"
if [ $? -eq 0 ]; then
  if [ -n "$out" ]; then
    pass "agent whoami answered an identity: $out"
  else
    fail "agent whoami exited 0 with no identity"
  fi
elif grep -q "agent identity unavailable" "$TMP/who.err"; then
  pass "agent whoami refused clearly (no launcher-stamped identity here)"
else
  fail "agent whoami: neither an identity nor the clear refusal: $(cat "$TMP/who.err")"
fi

# weave fleet: PATH evidence reads 'installed', never 'available'.
# Needs a git worktree to run in; use a throwaway repo when git exists.
if command -v git >/dev/null 2>&1; then
  gitdir="$TMP/repo"
  git -c init.defaultBranch=main init -q "$gitdir" &&
  out="$(cd "$gitdir" && "$BIN" weave fleet --fleet sh 2>&1)"
  status="$(printf '%s\n' "$out" | awk '$1 == "sh" { print $2; exit }')"
  if [ "$status" = "installed" ] || [ "$status" = "NOT" ]; then
    pass "weave fleet reports PATH evidence as '$status', not 'available'"
  else
    fail "weave fleet status for an unprobed tool was '$status' (want installed/NOT FOUND): $out"
  fi
else
  echo "SKIP  weave fleet behavior (no git on this host)"
fi

# inbox/notify round-trip — only meaningful once both are mounted.
if "$BIN" notify --help >/dev/null 2>&1 && "$BIN" inbox --help >/dev/null 2>&1; then
  mkdir -p "$BASHY_MB_DIR/seen"
  printf '1\n' >"$BASHY_MB_DIR/seen/audit-reader"
  if "$BIN" notify --as audit-sender audit-reader "AUDIT-PING-1" >/dev/null 2>&1 &&
     "$BIN" inbox --as audit-reader 2>/dev/null | grep -q "AUDIT-PING-1"; then
    pass "notify → inbox round-trip"
  else
    fail "notify → inbox round-trip did not deliver"
  fi
fi

echo
if [ "$fails" -gt 0 ]; then
  echo "comms-cli-audit: $fails FAILURE(S) on $BIN — each one is a verb this binary ships but does not honor" >&2
  exit 1
fi
echo "comms-cli-audit: all checks passed on $BIN"
