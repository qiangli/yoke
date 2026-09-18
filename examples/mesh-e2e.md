---
name: mesh-e2e
description: Run an e2e suite on a bigger paired host; orchestrate locally
default: e2e
---

# Mesh e2e — drive locally, run the heavy suite on a bigger host

Orchestrate on this machine; run the memory-hungry e2e on `${HOST}` (a paired
big box) over `--mesh`. The `ssh-ready` preflight verifies passwordless SSH
first and, on failure, prints how to fix it instead of hanging on a password
prompt or failing cryptically mid-run.

```bash
# fail-fast preflight (default): clear instructions if ssh isn't ready
bashy dag --mesh examples/mesh-e2e.md e2e HOST=bigbox REPO=you/repo REF=main

# wait-for-resolution: poll ~5 min while you run ssh-copy-id in another shell
bashy dag --mesh examples/mesh-e2e.md e2e HOST=bigbox REPO=you/repo SSH_RETRIES=30

# custom ssh options (port, key): set the transport, Host: stays the target
bashy dag --mesh --remote "ssh -p 2222" examples/mesh-e2e.md e2e HOST=bigbox REPO=you/repo
```

`--mesh` sends `Host:`-tagged targets to another machine (transport `--remote`,
default `ssh`); `Host:` is the target host. Without `--mesh`, `Host:` is just
recorded and everything runs locally.

## Tasks

### ssh-ready
Verify passwordless SSH to ${HOST} before any remote work. Runs LOCALLY (no
Host:). Skipped when HOST is unset (then e2e runs locally). Set SSH_RETRIES=N to
poll (N attempts, 10s apart) while you set up the key in another shell; default 0
fails fast with a remediation recipe.
When: test -n "${HOST}"
Effects: net
```bash
host="${HOST:?set HOST=<your big box>}"
attempts="${SSH_RETRIES:-0}"
i=0
while :; do
  if ssh -o BatchMode=yes -o ConnectTimeout=5 -o StrictHostKeyChecking=accept-new "$host" true 2>/dev/null; then
    echo "ssh ok: passwordless access to $host"
    exit 0
  fi
  if [ "$i" -ge "$attempts" ]; then
    echo "cannot reach '$host' without a password." >&2
    echo "set up passwordless ssh once, then it just works:" >&2
    echo "  ssh-keygen -t ed25519        # only if you have no key yet" >&2
    echo "  ssh-copy-id $host            # installs your public key on $host" >&2
    echo "  ssh $host true               # confirm it no longer prompts" >&2
    exit 1
  fi
  i=$((i + 1))
  echo "waiting for ssh to $host (attempt $i/$attempts) — fix it in another shell..." >&2
  sleep 10
done
```

### e2e
Run the e2e suite on ${HOST}. Requires ssh-ready, so a broken connection skips
this instead of hanging. The body fetches its own code from GitHub (the DATA
plane); only status flows back over the control plane.
Requires: ssh-ready
Host: ${HOST}
Effects: net, write
```bash
set -e
git clone --depth 1 -b "${REF:-main}" "https://github.com/${REPO:?set REPO=owner/name}" ws
cd ws
echo "running e2e on $(hostname)"
go test ./e2e/... || make test-e2e
```
