#!/usr/bin/env bash
# Register command-check.bsh as the `bashy command-check` command and skill.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
runner=${BASHY_BIN:-bashy}
guide=$("$runner" --bashsharp "$here/command-check.bsh" --skill)
"$runner" commands add command-check --force \
  --set "exec.0=$runner" --set exec.1=--bashsharp --set "exec.2=$here/command-check.bsh" \
  --set synopsis='Check one command against the bashy command contract and report each requirement by ID' \
  --set "long=$guide" --set group=diagnostics \
  --set effects.0=read --set effects.1=exec
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/command-check"
printf '%s\n' "$guide" > "$tmp/command-check/SKILL.md"
"$runner" skill add "$tmp/command-check" --force
echo 'Registered command-check command and skill.'
