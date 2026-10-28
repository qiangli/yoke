# S515.9 — Windows host runner

Story #2774 (`7e6a61753680`), Sprint #515. Spec: umbrella
`docs/sprint-515-satellite-loom.md`; prerequisite: cloudbox S515.6 and its
`docs/s515-6-ci-on-peers.md`. Live verification: 2026-10-28 UTC.

**A loom push ran green on native Windows, using `windows:host`.** The
runner was stopped afterward. This delivers explicit local-build support;
automatic signed Windows installation still needs a fork release.

## Why Windows was refused

`external/actrunner/actrunner.go` rejected `runtime.GOOS == "windows"` in
`resolveTool`, before binmgr ran. Its signed pin `bashy-v5.0.0-1` has only
Linux and macOS assets. The fork `qiangli/act_runner`, `bashy-build` commit
`d0baf282565f25c73f107f9887582fcd1fc36b52`, confirms the same four-platform
matrix in `.github/workflows/bashy-build.yml` and `bashy-build/PIN`.
Those files are generated from umbrella `bashy/builds/act_runner/tool.env`.

This is a packaging restriction, not an upstream Windows refusal. The fork
builds the pristine MIT-licensed `gitea.com/gitea/runner` v5.0.0 source at
`8b14a38d4ad6a1cb40dabb953a8a65656de70301`. That source compiled and ran on
Windows without any source patch. Its host executor already handles Windows
paths. No replacement executor, Docker, WSL runner, git shim, or fork source
change was needed for this smoke job.

## Small change and its boundary

`ACT_RUNNER_BIN` explicitly selects an absolute local executable for the
existing path/register/daemon functions. Invalid paths fail; a local binary
cannot be combined with a version selection. The path command identifies it
as a local build, not as a verified release. No PATH discovery or unsigned
network fallback was added. Without this variable the signed-download policy,
including the refusal on Windows, remains intact.

The caller owns the selected executable's provenance. For this test it was
built with `bashy go` from the exact pristine upstream commit in the fork's
PIN, using the fork's Go 1.27.2 security override and release version stamp:

```sh
# In a clean checkout of the pinned upstream source:
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 bashy go build -trimpath \
  -ldflags '-s -w -X gitea.com/gitea/runner/internal/pkg/ver.version=v5.0.0' \
  -o act_runner.exe .
```

Observed executable SHA256:
`4ffe885f433d550ec5ff5d1592c656bef3ecc957034d11f899841b4d7c7b3ad2`.
This is a local-build digest, **not a Sigstore attestation**. The live front
door was a small Windows executable calling this checkout's
`actrunner.NewCmd().Execute()`, built with `bashy go`; the installed bashy was
not replaced. Workflow steps used the peer's installed bashy rc.21-dev.

For automatic installation, the release owner must add Windows assets to
`MATRIX`, `GO_CGO`, `RELEASE_ASSETS`, and `RELEASE_EXECUTABLES` in the existing
build declaration, regenerate the fork workflow, pass its gates, and publish
a new signed tag. Only then can yoke pin the real Windows hashes/bundles and
remove the managed Windows guard. No release was pushed in this worker run.

## Connection and registration

All Windows test files lived under `C:/s515/win-runner`. Access came from the
sprint host claims and used `bashy outpost ssh`; host names, peer IDs and
credentials are deliberately not recorded here.

The existing S515.6 loom remained on its peer's `127.0.0.1:31880`. That peer's
Windows outpost can reach the WSL loopback listener via localhost forwarding.
A temporary mesh exposure forwarded it to the Windows runner's loopback:

```sh
# On the loom peer's Windows side:
bashy outpost mesh expose s515-win-loom 127.0.0.1:31880
# On the runner peer (PEER_ID from the loom peer's mesh status):
bashy outpost mesh listen "$PEER_ID" s515-win-loom --local 127.0.0.1:31880
```

`mesh dial s515-win-loom` discovered the service from the controller, but
reported no reachable service from the Windows runner. Selecting the same
already-connected peer with `mesh listen` worked. Mesh status reported a
**direct LAN QUIC** connection; traffic was not relayed through cloudbox.
`GET http://127.0.0.1:31880/api/v1/version` on the runner returned `28.1.0`.

Registration used the loom admin API's **POST**
`/api/v1/admin/actions/runners/registration-token`, then the existing wrapper:

```sh
export ACT_RUNNER_BIN=C:/s515/win-runner/act_runner.exe
bashy act-runner register --data C:/s515/win-runner \
  --instance http://127.0.0.1:31880 --token "$REG" \
  --name s515-windows-host --labels windows:host
bashy act-runner daemon --data C:/s515/win-runner
```

For the live candidate, substitute `C:/s515/win-runner/runner.exe` for
`bashy act-runner`. `config.yaml` in that directory contained:

```yaml
log:
  level: info
runner:
  capacity: 1
  envs:
    TMP: C:/s515/win-runner/tmp
    TEMP: C:/s515/win-runner/tmp
cache:
  enabled: false
  host: 127.0.0.1
host:
  workdir_parent: C:/s515/win-runner/work
```

The daemon also inherited TMP/TEMP under the scratch directory. The cache
was disabled because this job needs none. Explicit `shell: bashy {0}` uses
the existing shell and in-process commands; no dependency on PowerShell or
an independently installed Go/git executable is assumed.

## Pushed workflow and evidence

A new private scratch repository `root/s515-windows-host` on the existing
loom received commit `a8af5cc5f53273cbff3b5ad450fc20db2ca19c07` on `main`.
Its `.gitea/workflows/windows.yml` was:

```yaml
name: Windows host smoke
on: [push]
jobs:
  smoke:
    runs-on: windows
    defaults:
      run:
        shell: bashy {0}
    steps:
      - name: Bashy and native Go build
        run: |
          set -eu
          bashy --version
          bashy go version
          test "$(bashy go env GOOS)" = windows
          printf 'package main\nimport "fmt"\nfunc main() { fmt.Println("S515.9 Windows host OK") }\n' > smoke.go
          bashy go build -o smoke.exe smoke.go
          ./smoke.exe
```

Read back from the loom API, rather than inferred from a command exit code:

| Field | Observed value |
| --- | --- |
| Run | 67, run number 1 |
| Event | push |
| Job | 124, `smoke` |
| Runner | ID 3, `s515-windows-host` |
| Label | `windows` (registered as `windows:host`) |
| Run and job status | completed |
| Run, job and step conclusion | success |

Job log `/api/v1/repos/root/s515-windows-host/actions/jobs/124/logs`:

```text
2026-10-28T06:02:51.4041311Z windows/amd64
2026-10-28T06:02:51.5718594Z bashy, GNU Bash 5.3 compatible, version 5.3.0(1)-bashy-v1.0.0-rc.21-dev
2026-10-28T06:02:51.6897440Z go version go1.27.2 windows/amd64
2026-10-28T06:02:56.1178199Z S515.9 Windows host OK
2026-10-28T06:02:56.5093512Z Job succeeded
```

The smoke deliberately has no `uses:` actions or checkout step. It proves
native host execution and Go compilation, not every third-party action's
Windows compatibility.

## Gates and cleanup

- `bashy go test ./external/actrunner`: PASS, including explicit local-build
  resolution, invalid paths, conflicting versions and existing signed-pin tests.
- `GOOS=windows bashy go vet ./external/actrunner`: PASS.
- Cross-compiled the package test binary with `bashy go test -c`, transferred
  it through outpost SSH into the scratch directory, and ran it natively on
  Windows: all 11 tests PASS, including local executable selection.
- After the green job, terminated only the process whose full executable path
  matched `C:/s515/win-runner/act_runner.exe`. A bashy-built Windows Go helper
  used the Windows process APIs (no host shell command), waited for exit,
  then enumerated again: **scratch runner process count: 0**. The wrapper's
  waiting SSH session exited too.
- Removed this run's temporary mesh listeners and exposure after verification;
  left the pre-existing loom and Linux runner untouched. The private scratch
  repository retains the workflow run and logs for review.
