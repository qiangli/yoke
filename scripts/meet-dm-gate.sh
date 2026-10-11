#!/usr/bin/env bash
# meet-dm-gate.sh — real-process gate for the messenger front door
# (`bashy meet dm`): pairing, scoped delivery, /approve on the shared S4 queue,
# revoke/expiry refusal, and the S3 timeline, all against a BUILT binary under
# an isolated home. Pair codes are never printed; the receipt carries categories.
#
#   MEET_DM_BIN=/path/to/bashy scripts/meet-dm-gate.sh
#
# Default binary: the commscli audit tool (this tree's test binary re-exec'd with
# COMMSCLI_TOOL=1), which mounts meet + app + approve + events exactly as a host
# does. The agent is a deterministic stub tool planted in an isolated fleet, so
# the gate needs no model account; it does not prove a channel bot account.
#
# Exit 0 = every checked contract held.

set -u
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

BIN="${MEET_DM_BIN:-}"
if [ -z "$BIN" ]; then
	(cd "$ROOT" && GOWORK=off go test -c -o "$TMP/audit.test" ./test/commscli) || { echo "meet-dm-gate: build failed" >&2; exit 2; }
	BIN="$TMP/audit.test"
	export COMMSCLI_TOOL=1
fi

export HOME="$TMP/home"
export BASHY_HOME="$TMP/bashy"
export BASHY_MEET_DIR="$TMP/meet"
export BASHY_ROOM_DIR="$TMP/room"
export BASHY_MB_DIR="$TMP/mb"
export BASHY_FLEET_DIR="$TMP/fleet"
export BASHY_ASK_DIR="$TMP/ask"
export BASHY_FLEET_SEEDS=off
export PATH="$TMP/bin:/usr/bin:/bin"
unset BASHY_PRINCIPAL BASHY_AGENT_ID BASHY_AGENT WEAVE_AGENT BASHY_EPISODE WEAVE_EPISODE BASHY_AGENTIC 2>/dev/null || true
mkdir -p "$HOME" "$BASHY_HOME" "$BASHY_MEET_DIR" "$BASHY_ROOM_DIR" "$BASHY_MB_DIR" "$TMP/bin" "$BASHY_FLEET_DIR/tools" "$BASHY_FLEET_DIR/agents"

# The pinned agent: a stub CLI that echoes a fixed marker plus its prompt tail.
cat >"$TMP/bin/gate-agent" <<'EOF'
#!/bin/sh
echo "GATE-AGENT-REPLY ran"
EOF
chmod +x "$TMP/bin/gate-agent"
cat >"$BASHY_FLEET_DIR/tools/gate-agent.yaml" <<'EOF'
name: gate-agent
kind: cli
cli:
  binary: gate-agent
  launch:
    exec: gate-agent --model {model} -p {prompt}
EOF
cat >"$BASHY_FLEET_DIR/agents/gatebot.yaml" <<'EOF'
agents:
  - name: gatebot
    tool: gate-agent
    model: stub-model
    display: "gatebot"
EOF

fails=0
pass() { printf 'PASS  %s\n' "$1"; }
fail() { printf 'FAIL  %s\n' "$1"; fails=$((fails + 1)); }
run() { "$BIN" "$@" 2>"$TMP/err"; }
redact() { sed -E 's/[A-Z2-7]{4}-[A-Z2-7]{4}/<code>/g'; }

CH=slack
code_of() { grep -oE '[A-Z2-7]{4}-[A-Z2-7]{4}' | head -1; }
count_inbound() { run events --json --kind inbound | grep -c "\"actor\":\"$CH:$1\""; }

# 1. unknown sender: one code, no agent launched.
out="$(run meet dm gatebot --channel $CH --sender alice "hello there")"
CODE="$(printf '%s' "$out" | code_of)"
if [ -n "$CODE" ] && ! printf '%s' "$out" | grep -q GATE-AGENT-REPLY; then
	pass "unpaired sender gets a pair code and no agent runs   [$(printf '%s' "$out" | redact | cut -c1-90)]"
else fail "unpaired sender: $out"; fi
out="$(run meet dm gatebot --channel $CH --sender alice "hello again")"
if printf '%s' "$out" | grep -q "pairing pending" && [ -z "$(printf '%s' "$out" | code_of)" ]; then
	pass "second message from the same sender is pending, no second code"
else fail "second message: $out"; fi

# 2. approve: ask+approve scope; code is single use.
out="$(run app pair approve "$CODE" --allow ask --allow approve)"
printf '%s' "$out" | grep -q "paired alice on channel $CH" && pass "operator approves the code (ask+approve)" || fail "approve: $out"
run app pair approve "$CODE" >/dev/null 2>&1 && fail "pair code redeemed twice" || pass "pair code is single use"

# 3. the same message now reaches the agent; reply on stdout.
out="$(run meet dm gatebot --channel $CH --sender alice "hello there")"
printf '%s' "$out" | grep -q "GATE-AGENT-REPLY" && pass "paired sender gets the agent reply on stdout" || fail "paired delivery: $out / $(cat "$TMP/err")"
out="$(printf '{"text":"via stdin json","thread":"T1"}' | run meet dm gatebot --channel $CH --sender alice)"
printf '%s' "$out" | grep -q "GATE-AGENT-REPLY" && pass "JSON on stdin with --thread-style payload is accepted" || fail "stdin json: $out"

# 4. S4: a real approval raised, resolved by /approve ID with the same id.
rq="$(run approve request -- git push origin main)"
AID="$(printf '%s' "$rq" | grep -oE '[0-9a-f]{8,}' | head -1)"
if [ -z "$AID" ]; then fail "approve request gave no id: $rq / $(cat "$TMP/err")"; else
	run approve list | grep -q "${AID:0:8}" && pass "approval $AID is pending in the shared queue" || fail "approval not listed"
	out="$(run meet dm gatebot --channel $CH --sender alice "/approve $AID")"
	if printf '%s' "$out" | grep -q "allow $AID"; then pass "/approve resolves the same approval id"; else fail "/approve: $out / $(cat "$TMP/err")"; fi
	run approve list | grep -q "${AID:0:8}" && fail "approval still pending" || pass "approval left the pending queue"
	run events --json --kind approval | grep -q "${AID:0:8}" && pass "approval decision is on the timeline" || fail "no approval event for $AID"
fi

# 5. an ask-only sender cannot settle approvals.
out="$(run meet dm gatebot --channel $CH --sender bob "hi")"; BCODE="$(printf '%s' "$out" | code_of)"
run app pair approve "$BCODE" --allow ask >/dev/null
rq="$(run approve request -- rm -rf build)"; BID="$(printf '%s' "$rq" | grep -oE '[0-9a-f]{8,}' | head -1)"
out="$(run meet dm gatebot --channel $CH --sender bob "/approve $BID")"
printf '%s' "$out" | grep -q 'needs scope "approve"' && run approve list | grep -q "${BID:0:8}" \
	&& pass "ask-only sender cannot /approve; approval stays pending" || fail "scope: $out"

# 6. revoke refuses later delivery.
run meet dm --channel $CH --sender alice --revoke | grep -q "revoked alice" && pass "revoke ends the pairing" || fail "revoke"
out="$(run meet dm gatebot --channel $CH --sender alice "hello there")"
if [ -n "$(printf '%s' "$out" | code_of)" ] && ! printf '%s' "$out" | grep -q GATE-AGENT-REPLY; then
	pass "revoked sender is sent back to pairing; no agent runs"
else fail "after revoke: $out"; fi

# 7. expiry refuses later delivery.
out="$(run meet dm gatebot --channel $CH --sender carol "hi")"; CCODE="$(printf '%s' "$out" | code_of)"
run app pair approve "$CCODE" --allow ask --ttl 2s >/dev/null
run meet dm gatebot --channel $CH --sender carol "hi" | grep -q GATE-AGENT-REPLY && pass "carol delivers inside the ttl" || fail "carol inside ttl"
sleep 3
out="$(run meet dm gatebot --channel $CH --sender carol "hi")"
if [ -n "$(printf '%s' "$out" | code_of)" ] && ! printf '%s' "$out" | grep -q GATE-AGENT-REPLY; then
	pass "expired pairing refuses delivery and re-pairs"
else fail "after expiry: $out"; fi

# 8. S3: every message is one inbound event.
n=$(count_inbound alice)
# alice: unpaired, pending, paired, stdin json, /approve, post-revoke = 6
[ "$n" = 6 ] && pass "timeline: 6 inbound events for $CH:alice" || fail "timeline events for alice = $n, want 6"

printf '\n%d failure(s)\n' "$fails"
exit $((fails > 0))
