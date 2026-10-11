---
name: agent-hub
description: Human notification sinks and external messenger dispatch (desktop, ntfy, webhook)
metadata:
  requires: "has=bashy"
---

# agent-hub — human notification sinks outside bashy

`agent-hub` provides external delivery sinks for bashy bus subscriptions and events streams.
When bus sidecar subscriptions or `bashy events` emit events requiring human attention
(such as operator approvals, sprint blockers, or critical notices), these Bash# functions
deliver them to channels outside bashy.

## Constraints & Security

- **Subject-only:** Only the explicit `subject`/`summary` field and event coordinates (type, target)
  leave the host. The sinks never read `body`: bodies, full transcripts, and sensitive operation
  payloads never leave the host, and an event with no subject is delivered as its type/target title.
- **Behind exec:** Designed to be run as an argv target from `--sink exec -- ...`, receiving
  the event JSON on stdin, or from a piped stream like `bashy events --follow --kind approval,notice --json`.

## Bash# Functions

`meet_dm` is the inbound adapter used by gateway hooks (OpenClaw, Codync,
ycode, or a bot script). The gateway supplies its channel and stable sender
id; store gateway tokens with `bashy secret set NAME` and let the gateway
resolve them with `bashy secret get NAME`, not from this skill or a checked-in
config. EVERY message — including `/approve`, `/deny` and `/meet` — enters the
`bashy meet dm` front door, which records the S3 event and checks the
sender's app-pair device before anything happens. The router only picks the
agent; it never decides on its own.

~~~bash
# Route one inbound message. Set MEET_CHANNEL and MEET_SENDER from the
# gateway's account. MEET_AGENT is optional: left empty, the front door
# routes to the sender's S2 fleet binding.
meet_dm() {
	local text="$*" agent="${MEET_AGENT:-}" rest
	[ -n "$MEET_CHANNEL" ] && [ -n "$MEET_SENDER" ] || return 2
	case "$text" in
		/approve\ *|/deny\ *|/meet\ *)
			# Front-door commands: scope approve settles the shared approval
			# queue, scope control posts into a room. No agent is involved.
			agent="" ;;
		/status)  text="/status" ;;
		@*)
			rest="${text#@}"; agent="${rest%% *}"; text="${rest#* }"
			[ -n "$text" ] || return 2 ;;
	esac
	bashy meet dm "$agent" --channel "$MEET_CHANNEL" --sender "$MEET_SENDER" "$text"
}
~~~

An unpaired sender receives a single-use code and the message is not
delivered. The operator admits it with the app-pair store — `bashy app pair
approve CODE --allow ask|control|approve [--ttl 24h]` (or `bashy app pair deny
CODE`) — and the sender retries the same message; the agent reply is returned
on stdout. `ask` talks to an agent read-only, `control` runs with ordinary
turn authority and may `/meet ROOM TEXT`, `approve` may `/approve ID` and
`/deny ID`. `bashy app devices` lists paired senders; `bashy app revoke ID` or
`bashy meet dm --channel C --sender S --revoke` ends one.

~~~bash
# agent_hub_subject prints the explicit subject of one event JSON on stdin:
# .subject, else .summary, string-valued only, clipped to one line / 256 bytes.
# It never reads .body (or any other field): bodies carry operation and message
# contents and must not leave the host. No subject prints nothing, and the
# caller falls back to its type/target title.
agent_hub_subject() {
	jq -r '(.subject // .summary // "") | if type == "string" then . else "" end' 2>/dev/null \
		| head -n 1 | cut -c 1-256
}

# desktop delivers a notification to the local desktop environment.
# Uses osascript on macOS and notify-send on Linux/FreeBSD.
desktop() {
	local input
	input="$(cat)"
	[ -z "$input" ] && return 0

	local kind target subject title
	kind="$(printf '%s' "$input" | jq -r '.type // .kind // "notice"' 2>/dev/null || echo "notice")"
	target="$(printf '%s' "$input" | jq -r '.target // ""' 2>/dev/null || true)"
	subject="$(printf '%s' "$input" | agent_hub_subject)"

	if [ -n "$target" ]; then
		title="bashy [$kind] $target"
	else
		title="bashy [$kind]"
	fi

	[ -z "$subject" ] && subject="$title"

	if command -v osascript >/dev/null 2>&1; then
		osascript - "$title" "$subject" <<'APPLESCRIPT'
on run argv
	display notification (item 2 of argv) with title (item 1 of argv)
end run
APPLESCRIPT
	elif command -v notify-send >/dev/null 2>&1; then
		notify-send "$title" "$subject"
	else
		printf 'agent-hub: neither osascript nor notify-send is available\n' >&2
		return 127
	fi
}

# ntfy delivers a subject notification via ntfy.sh or a self-hosted ntfy instance.
# Configured via NTFY_URL or NTFY_TOPIC.
ntfy() {
	local input
	input="$(cat)"
	[ -z "$input" ] && return 0

	local kind target subject title topic_url
	kind="$(printf '%s' "$input" | jq -r '.type // .kind // "notice"' 2>/dev/null || echo "notice")"
	target="$(printf '%s' "$input" | jq -r '.target // ""' 2>/dev/null || true)"
	subject="$(printf '%s' "$input" | agent_hub_subject)"

	if [ -n "$target" ]; then
		title="bashy [$kind] $target"
	else
		title="bashy [$kind]"
	fi
	[ -z "$subject" ] && subject="$title"

	topic_url="${NTFY_URL:-}"
	if [ -z "$topic_url" ] && [ -n "${NTFY_TOPIC:-}" ]; then
		topic_url="https://ntfy.sh/$NTFY_TOPIC"
	fi

	if [ -z "$topic_url" ]; then
		printf 'agent-hub: set NTFY_URL or NTFY_TOPIC\n' >&2
		return 2
	fi
	command -v curl >/dev/null 2>&1 || { printf 'agent-hub: curl is required for ntfy\n' >&2; return 127; }
	curl --fail --silent --show-error -H "Title: $title" -d "$subject" "$topic_url" >/dev/null
}

# webhook delivers a subject notification payload via HTTP POST to WEBHOOK_URL.
webhook() {
	local input
	input="$(cat)"
	[ -z "$input" ] && return 0

	local kind target subject title url
	kind="$(printf '%s' "$input" | jq -r '.type // .kind // "notice"' 2>/dev/null || echo "notice")"
	target="$(printf '%s' "$input" | jq -r '.target // ""' 2>/dev/null || true)"
	subject="$(printf '%s' "$input" | agent_hub_subject)"

	if [ -n "$target" ]; then
		title="bashy [$kind] $target"
	else
		title="bashy [$kind]"
	fi
	[ -z "$subject" ] && subject="$title"

	url="${WEBHOOK_URL:-}"
	if [ -z "$url" ]; then
		printf 'agent-hub: set WEBHOOK_URL\n' >&2
		return 2
	fi
	command -v jq >/dev/null 2>&1 || { printf 'agent-hub: jq is required for webhook\n' >&2; return 127; }
	command -v curl >/dev/null 2>&1 || { printf 'agent-hub: curl is required for webhook\n' >&2; return 127; }
	local payload
	payload="$(jq -n --arg title "$title" --arg kind "$kind" --arg target "$target" --arg subject "$subject" \
		'{title: $title, kind: $kind, target: $target, subject: $subject}')" || return
	curl --fail --silent --show-error -H "Content-Type: application/json" -d "$payload" "$url" >/dev/null
}

# agent_hub_dispatch reads line-delimited events JSON (e.g. from bashy events --follow --json)
# and routes each event to the specified sink (default: desktop).
agent_hub_dispatch() {
	local sink="${1:-desktop}"
	while IFS= read -r line || [ -n "$line" ]; do
		[ -z "$line" ] && continue
		case "$sink" in
			desktop) printf '%s\n' "$line" | desktop ;;
			ntfy)    printf '%s\n' "$line" | ntfy ;;
			webhook) printf '%s\n' "$line" | webhook ;;
			*)       printf '%s\n' "$line" | desktop ;;
		esac
	done
}
~~~

## Usage

Behind an exec sink on bus sidecar:
```bash
bashy bus subscribe --as operator --kind approval,sprint \
    --sink exec -- bashy -lc desktop
```

Piped from `bashy events`:
```bash
bashy events --follow --kind approval,notice --json | bashy -lc 'agent_hub_dispatch desktop'
```

Install the fenced functions in the supervised Bash# process's startup file
before using these recipes. A non-zero function exit is intentional: the
sidecar records a failed sink as a local queued delivery instead of claiming an
external notification succeeded.
