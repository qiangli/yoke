#!/usr/bin/env bash
# Smoke test: the conforming hello candidate passes; `true` does not.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
runner=${BASHY_BIN:-bashy}
work=$(mktemp -d); trap 'rm -rf "$work"' EXIT
mkdir -p "$work/public"
printf '<main><h1 id="greeting">Hello, World!</h1></main>\n' > "$work/public/index.html"
cp -R "$here/hello-world-candidate/bin" "$work/"
check() { "$runner" --bashsharp "$here/command-check.bsh" "$@"; }
check -- "$runner" --bashsharp "$work/bin/hello.bsh" >/dev/null
if check -- true >/dev/null 2>&1; then echo 'FAIL: true passed the contract check' >&2; exit 1; fi
out=$(check --json -- true 2>&1 >/dev/null || true)
[[ $out == *'"status": "error"'* || $out == *'"status":"error"'* ]] || { echo 'FAIL: no error envelope' >&2; exit 1; }
"$runner" --bashsharp "$here/command-check.bsh" --fields x -- true 2>/dev/null && exit 1 || [[ $? == 2 ]]
echo PASS
