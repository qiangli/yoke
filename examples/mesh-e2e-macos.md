---
name: mesh-e2e-macos
description: Run a test/e2e suite on a paired macOS host, orchestrated locally
default: e2e
vars:
  HOST: bigbox
---

# Mesh e2e on a paired macOS host

Drive the run on this machine; execute the suite on the remote host (`${HOST}`)
over `--mesh`.

```bash
bashy dag --mesh examples/mesh-e2e-macos.md                 # default goal: e2e
bashy dag --mesh examples/mesh-e2e-macos.md SSH_RETRIES=30  # wait while you set up ssh
bashy dag --mesh examples/mesh-e2e-macos.md HOST=other-box  # override the host
```

How it works:
- `bashy` is NOT needed on the remote host — the mesh runs the body in the
  remote's own `bash`; only this (orchestrator) host runs `bashy`.
- Code is pulled from **GitHub directly** by the worker (the data plane); the SSH
  control channel carries only the command + output.
- `ssh-ready` checks connectivity (locally); the `e2e` target's builtin
  **`Tools:` preflight** checks `git`/`podman` exist on the remote host before
  doing any work — a missing tool fails with `dag: required tool not found: …`.
- The workspace persists at `~/dag-ws/<repo>` on the remote host (idempotent clone).
- Edit `REPO`/`REF`/`SUITE` in the `e2e` body for your suite.

> `Tools:` runs against the remote's non-login PATH (so `git`, `podman`, system
> tools). A version-manager toolchain (mise/asdf — e.g. `go` here) isn't on that
> PATH, so the body activates it (`mise shims`) and the suite uses it.

## Tasks

### ssh-ready
Verify passwordless SSH to ${HOST} before any remote work (runs locally). Skipped
when HOST is unset (then e2e runs locally). SSH_RETRIES=N polls N×10s while you
`ssh-copy-id` in another shell; default 0 fails fast with a recipe.
When: test -n "${HOST}"
Effects: net
```bash
host="${HOST:?set HOST=<host>}"; attempts="${SSH_RETRIES:-0}"; i=0
while :; do
  if ssh -o BatchMode=yes -o ConnectTimeout=5 -o StrictHostKeyChecking=accept-new "$host" true 2>/dev/null; then
    echo "ssh ok: passwordless access to $host"; exit 0
  fi
  if [ "$i" -ge "$attempts" ]; then
    echo "cannot reach '$host' without a password." >&2
    echo "  ssh-keygen -t ed25519   # only if you have no key" >&2
    echo "  ssh-copy-id $host       # install your key on $host" >&2
    exit 1
  fi
  i=$((i + 1)); echo "waiting for ssh to $host ($i/$attempts)..." >&2; sleep 10
done
```

### e2e
Run the suite on ${HOST}. Requires ssh-ready (broken connection → skipped). The
builtin Tools: preflight verifies git/podman exist on the host first; then the
body activates the mise toolchain, clones from GitHub, and runs the suite.
Requires: ssh-ready
Host: ${HOST}
Tools: git podman
Effects: net, write
```bash
set -e
# mise-managed toolchain (go/node) — not on the non-login PATH, so activate it:
export PATH="$HOME/.local/share/mise/shims:/opt/homebrew/bin:$PATH"
command -v go >/dev/null || { echo "go not found (install: mise use -g go@latest)" >&2; exit 3; }

# --- EDIT for your suite -------------------------------------------------
REPO="you/repo"               # GitHub owner/name
REF="main"
SUITE="go test ./..."         # your e2e command
# ------------------------------------------------------------------------

WS="$HOME/dag-ws/${REPO##*/}"   # persistent, inspectable on the host
if [ -d "$WS/.git" ]; then
  git -C "$WS" fetch --depth 1 origin "$REF"
  git -C "$WS" checkout -q --force FETCH_HEAD
else
  mkdir -p "$(dirname "$WS")"
  git clone --depth 1 -b "$REF" "https://github.com/$REPO" "$WS"
fi
cd "$WS"
echo "==> $(hostname) ($(sysctl -n hw.ncpu) cpu / $(( $(sysctl -n hw.memsize)/1024/1024/1024 ))GB): $SUITE  [$WS]"
eval "$SUITE"
```
