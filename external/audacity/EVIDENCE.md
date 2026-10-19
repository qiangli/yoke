# audacity evidence — story 0e5f3dfcbb46 (sprint 382, issues #429, #439)

Yoke-side admission and verification record for the Audacity external.
Build/license admission, fixture tests, and live verified support are
distinct rows below; a compiling wrapper or a passing fake-pipe test
is NOT live support. windows/amd64 is pinned; darwin/* fails closed
(.dmg/.pkg only).

## Admission (reviewed)

- Candidate: Audacity — `audacity/audacity`, GPL-2.0-or-later. 3.x only.
- License posture: download + exec + same-user local pipe ONLY. Nothing
  from Audacity is linked, vendored, embedded, or modified; yoke invokes
  the unmodified official binary and talks to a running Audacity 3.x
  instance over its local mod-script-pipe. The fixed pipe command sets
  in `audacity.go` are yoke-authored strings sent BY the wrapper, not
  Audacity code.
- Pinned version: `DefaultVersion = "3.7.3"` (3.7 line). No "latest":
  a version change is a reviewed edit of the constant plus matching pins.
  Audacity 4 is explicitly rejected (`ErrUnsupportedVersion`, `ArtifactFor`
  returns false, `Spec` errors on major != 3).
- Official artifacts (`ArtifactFor`, `audacity.go`), release root
  `https://github.com/audacity/audacity/releases/download/Audacity-<ver>/`
  — the GitHub release that carries the audacity/audacity tags, same
  bytes and `CHECKSUMS.txt` GitHub publishes:
  - linux/amd64: `audacity-linux-3.7.3-x64-22.04.AppImage` (raw binary,
    `Tree: false`, no Entrypoint). Also `...-20.04.AppImage` exists but
    this story pins 22.04 only.
  - windows/amd64: `audacity-win-3.7.3-64bit.zip`, entrypoint
    `audacity-win-3.7.3-64bit/Audacity.exe` (Tree asset).
  - darwin/arm64, darwin/amd64: NOT DECLARED — Audacity publishes only
    `.dmg` and `.pkg`, which `pkg/binmgr` does not unpack. `ErrNoArtifact`.
  - linux/arm64, windows/arm64, windows/386: NOT DECLARED. `ErrNoArtifact`.
- Mode: short-lived same-user local mod-script-pipe session only
  (supply-chain Rule 2). Each operation opens the FIFO pair ONCE and sends
  its fixed command list (`Import2`/`Select`/`Trim`/`LoudnessNormalization`/
  `Export2`/`GetInfo`, quoted Filename, no free-form pipe input) in strict
  request/reply order — the protocol of Audacity's own reference client
  `au3/scripts/piped-work/pipe_test.py` (`do_command` = `send_command` +
  `get_response` per command) — under the caller's context (plus an
  optional `--timeout`). No network listener,
  no resident bridge/MCP server, no daemon, no detached child is admitted
  (`TestNoDaemonListenerOrNetwork`, pipe vocabulary tests). The pipe is a
  same-user local FIFO on Linux/Unix (`/tmp/audacity_script_pipe.to.<uid>`
  / `.from.<uid>`), verified via `syscall.Stat_t.Uid` same-user check; the
  wrapper never dials TCP. Windows is provision-only in this story:
  `defaultPipeClient` fails closed on `GOOS=windows` before any pipe open
  ("no running Audacity 3.x with mod-script-pipe") — Windows named-pipe
  transport (`\\.\pipe\ToSrvPipe`/`FromSrvPipe`) is NOT implemented here,
  only the artifact pin is committed (see Pins and Lifecycle).

## Pins (honest status)

- `pkg/binmgr/pins.go` carries entries:
  `audacity@3.7.3/windows/amd64 =
  1d4c91d0bc1b5376f4c8768d48a6e79ab9dcb4600c1162053f61556becb45ce5` and
  `audacity@3.7.3/linux/amd64 =
  4e7600c3d9f09dca52c7171cc0bfe76abb168436143cd02ecfa3052ab9f7aaa1`.
  Each value is Audacity's own published digest from
  `https://github.com/audacity/audacity/releases/download/Audacity-3.7.3/CHECKSUMS.txt`
  (fetched 2026-10-18 from the authoring host). Windows zip was also
  independently verified by downloading the 26,398,933-byte zip on the
  authoring host and sha256-hashing it locally — same value, not a mirror.
  `TestOfficialSourceAndPin` pins both constants so a drift is a test
  failure, and the assets declare no SHA512/MD5.
- darwin/* and */arm64: `ErrNoArtifact` (no binmgr-consumable archive).
  The release-supplied `CHECKSUMS.txt` is never consulted at provision time;
  only the committed pin is verified.
- `Spec` declares an asset only for a platform whose pin exists
  (`TestSpecAndPathFailClosedWithoutCommittedPin`, run against an empty
  `BASHY_BIN_CACHE`): windows/amd64 is "Tree with Entrypoint", linux/amd64
  is "raw binary"; every other platform has no asset, no network.

## The four operation boundaries

Every operation takes explicit ABSOLUTE paths (relative paths, control
characters, option-looking values, wrong extensions, missing inputs,
missing output parents are refused BEFORE provisioning or pipe —
`TestValidationRunsBeforeExecution`, `TestResolverFailureMeansNoPipe`)
and sends ONE fixed command set over the local pipe. Paths reach the
pipe only as quoted `Filename="..."` values. Each transaction returns
exactly one `yoke-audacity-v1 `-prefixed JSON line; the wrapper never
emits raw pipe output (`TestInfoPipeCommands`).

| Operation | Inputs | Output | Fixed pipe commands do |
|---|---|---|---|
| `info --input X.{wav,mp3,flac,ogg,aiff,aup3}` | existing audio/project | JSON (`yoke-audacity-v1`, op=info) | `Import2`, `GetInfo: Type=Tracks Format=JSON` |
| `trim --input X --output O.{wav,mp3,flac,ogg,aiff} --start S --end E` | existing audio, 0 ≤ S < E, interval ≥0.01s | writes `O.*`; JSON (`op=trim`) | `Import2`, `Select: Start=S End=E`, `Trim:`, `Export2` |
| `normalize --input X --output O --level L` | existing audio, L∈[-60,0] dB (default -3) | writes `O.*`; JSON (`op=normalize`) | `Import2`, `SelectAll:`, `LoudnessNormalization: Level=L`, `Export2` |
| `export --input X --output O [--format F]` | existing audio/project, F∈{wav,mp3,ogg,flac,aiff} matching O's ext | writes `O.*`; JSON (`op=export`) | `Import2`, `Export2` |

Exact command sets per operation are pinned by `TestInfoPipeCommands`,
`TestTrimPipeCommands`, `TestNormalizePipeCommands`, `TestExportPipeCommands`;
`TestPipeCommandsAreFixedSet` pins that every command is drawn from the
fixed vocabulary and carries no free-form content. `TestEffectsAreDeclared`
pins the operation effects (`info: read,process`; others: `read,write,process`).

## Unsupported modes (refused, not deferred)

- Free-form pipe input, arbitrary Audacity scripting IDs, positional
  pass-through (`TestNoFreeFormEscapeFlags`: unknown flags, positionals
  all fail, pipe never called).
- Audacity 4.x (`Spec` and `ArtifactFor` reject major !=3,
  `TestUnsupportedVersionRejected`).
- Any network listener: `audacity.go` imports no `net*`, `os/signal`,
  `os/exec`; contains no `Listen(`, `http.`, `Setsid`,
  `Setpgid`, `.Start(`, `binmgr.Launch/Start/RunSpec` (`TestNoDaemonListenerOrNetwork`).
  `syscall` and `golang.org/x/sys/unix` are used ONLY for `Stat_t.Uid`
  same-user FIFO verification (`verifyPipe` in `pipe_unix.go`) and for the
  raw `O_NONBLOCK` FIFO descriptors (`unix.Open/Read/Write/Close`) whose
  every wait is a `select` on `ctx.Done()`; no network or daemon is
  introduced. Windows `pipe_windows.go` imports no `syscall` for pipes.
- Host-installed Audacity: there is no `PreferHost` path; only the pinned,
  binmgr-verified artifact is ever provisioned.

## Local-pipe lifecycle

Audacity's mod-script-pipe is a same-user local IPC object, not a network
service. Lifecycle per operation (bounded, no resident):

1. Validate request (absolute paths, extensions, numeric bounds) — before
   any provision or pipe.
2. Provision Audacity via `Path` → `binmgr.Ensure` (download → sha256
   against committed pin → cache). If Audacity is not already running with
   `mod-script-pipe` enabled, provisioning still succeeds but the pipe step
   fails closed with "no running Audacity 3.x with mod-script-pipe".
3. Locate same-user pipes. On Linux/Unix: `/tmp/audacity_script_pipe.to.<uid>`
   and `.from.<uid>`, verified each is a FIFO owned by `os.Getuid()` via
   `syscall.Stat_t.Uid` (`verifyPipe` in `pipe_unix.go` — rejects non-FIFO
   and foreign-owner pipes, fail closed). On Windows: admitted as
   provision-only — `defaultPipeClient` fails closed before pipe open, so
   no `verifyPipe`/`open` is reached; Windows named-pipe transport is NOT
   implemented in this story (EVIDENCE.md declares it provision-only).
   No TCP dial, no cross-user pipe (`TestVerifyPipeRejectsNonFIFOAndWrongOwner`,
   `TestDefaultPipeClientRejectsRegularFileAsPipe`; Windows case covered by
   `TestSpecAndPathFailClosedWithoutCommittedPin` and the explicit
   `GOOS=windows` early return).
4. FIFO session (Unix only; Windows fails closed before this step) under
   the caller's `context.Context` (plus an inner 30 s default when the
   caller supplies no deadline), in the exact shape of the official
   `pipe_test.py` client:
   - open the `to` FIFO write-only ONCE (`O_NONBLOCK`; a FIFO with no
     reader fails with `ENXIO`, retried every 10 ms under `ctx` — no
     blocking open, no helper goroutine) and the `from` FIFO ONCE
     (`O_RDWR|O_NONBLOCK`, so the open never waits for a writer and an
     idle pipe reads `EAGAIN`, not EOF);
   - for EACH fixed command: write `command + "\n"`, then read THAT
     command's reply line by line through its `BatchCommand finished:`
     line (per-reply bound 64 KiB → `errReplyTooLarge`; leading blank
     lines are the previous reply's terminator and are skipped; a marker
     only counts at line start); `BatchCommand finished: Failed…` fails
     the operation immediately and the remaining commands are never
     written; otherwise continue with the next command;
   - the aggregate response is the concatenation of every per-command
     reply (one marker per command), bounded by the per-reply cap and the
     fixed command count;
   - both descriptors are raw `O_NONBLOCK` fds (`pipe_unix.go`): every
     `read`/`write` that would block returns `EAGAIN` and the retry is a
     `select` on `ctx.Done()` with a 2 ms tick, so cancellation is
     portable across Linux (FIFOs pollable) and darwin (FIFOs explicitly
     not pollable by Go's runtime — go issue #24164) and no goroutine is
     ever parked in the kernel.
   The PRIOR protocol — write every command up front as one batch, then
   read once until the first marker — is REJECTED (issue #439): Audacity's
   pipe server reads one line, executes it, replies, then reads the next,
   so a bulk write interleaves unread replies with pending commands,
   returns only the first reply, and keeps executing `Export2` after a
   failed `Import2`. Pinned by `TestPipeWireTransaction` (one line per
   command, 2 markers in the aggregate), `TestPipeStrictRequestReplyOrdering`
   (fake delays each reply 150 ms and timestamps arrivals: command N+1
   reaches the pipe strictly after reply N was sent),
   `TestPipeWithheldReplyRejectsSecondCommand` (ADVERSARIAL: the fake
   reads command 1 and never replies; the client times out on reply 1 and
   the fake drains the `to` FIFO to EOF proving command 2 was never
   written), `TestPipeFailedReplyAbortsBeforeNextCommand` (`Failed!` on
   command 1 → error `command 1/3 (Import2) failed`, nothing follows),
   `TestPipeReplyBoundIsEnforced`, `TestPipeWireBlankLineFraming` (split
   marker chunks), `TestPipeTransactionBoundedCancellation`,
   `TestOpenFileWithContextNoLeakedGoroutine`, `TestDeadlineReachesPipe`,
   `TestCallerContextCancellation`.
5. Build and emit one `yoke-audacity-v1` JSON line to `out`; no raw pipe
   data escapes (the envelope carries the first 2 KiB of the aggregate as
   `pipe_response`). Both FIFO ends are closed; no daemon remains.

Enabling mod-script-pipe (one-time, per Audacity install): Audacity →
Preferences → Modules → mod-script-pipe → Enabled → Restart. The wrapper
never enables it itself; it only uses an already-enabled instance. Tests
override `pipeDirOverride` to use `t.TempDir()` FIFOs (via `syscall.Mkfifo`)
and `fakeAudacity`, a fixture server with the real server's shape (read one
line → reply → read the next; a `nil` reply withholds forever), so the wire
protocol is fixture-verified without a live Audacity or display server;
live verification is `TestLive` (opt-in, below).

## Verification status

- Fixture tests (this repo, no network, no Audacity, no GUI/VM) — see [audacity_test.go](audacity_test.go):
  `gofmt -l` clean; `go vet` for GOOS=darwin/linux/windows; `go test
  -count=1 -race ./external/audacity ./pkg/binmgr` — PASS on darwin/arm64
  (authoring host, 2026-10-18, `go1.27.2 darwin/arm64`; issue #439 re-landing over
  accepted GIMP, pins.go keeps both `gimp@3.2.6` and `audacity@3.7.3`).
  These are SCOPED gates (`./external/audacity ./pkg/binmgr`): scoped
  tests, scoped vet, and GOOS=windows/linux/darwin vet passed; the whole
  `scripts/crossvet.sh` run is NOT claimed by this story's evidence.
  Fixture tests cover the bounded same-user local pipe (Unix FIFO
  admitted, Windows provision-only): `TestVerifyPipeRejectsNonFIFOAndWrongOwner`
  (FIFO type + `syscall.Stat_t.Uid` same-user), `TestPipeTransactionBoundedCancellation`
  (context deadline/cancel interrupts the FIFO open), `TestOpenFileWithContextNoLeakedGoroutine`
  (50 cancellations stay bounded, then a real session still succeeds),
  `TestPipeWireTransaction` (paired `syscall.Mkfifo` FIFOs in `t.TempDir()`
  with `pipeDirOverride`; one newline-terminated line per command; both
  replies aggregated), `TestPipeStrictRequestReplyOrdering` (command N+1
  written only after reply N read), `TestPipeWithheldReplyRejectsSecondCommand`
  (adversarial: reply 1 withheld → bounded timeout, command 2 never written),
  `TestPipeFailedReplyAbortsBeforeNextCommand` (`Failed!` aborts, names the
  command ID not the path), `TestPipeReplyBoundIsEnforced` (64 KiB per-reply
  cap), `TestPipeWireBlankLineFraming` (blank line before marker, marker
  split across chunks), `TestBatchFinishedLineAware` (marker only at line
  start), `TestDefaultPipeClientRejectsRegularFileAsPipe` (non-FIFO rejected
  before connect); plus `TestNoDaemonListenerOrNetwork` (allows `syscall`
  + `x/sys/unix` for UID/`O_NONBLOCK` only, still forbids `net*`,
  `os/signal`, `os/exec`, `Listen(`, `http.`, `binmgr.Launch/Start`),
  `TestOfficialSourceAndPin`, `TestUnsupportedVersionRejected`. No live
  Audacity process, VM, or container was started on the authoring host
  (fixture/unit work only).
- Live smoke `TestLive` ([live_test.go](live_test.go)) — opt-in
  (`YOKE_AUDACITY_LIVE=1`), production path only: `Path` → `binmgr.Ensure`
  against the committed pin, then the real cobra front door
  (`audacity info --input in.wav --timeout 2m`) → `defaultPipeClient` over
  the same-user FIFO session; asserts the `yoke-audacity-v1` envelope,
  two `BatchCommand finished: OK` markers (Import2 + GetInfo) and the
  GetInfo Tracks JSON. The input WAV is generated by the test with the
  stdlib. It never starts Audacity (no launch path exists): the operator
  starts the provisioned 3.7.3 with mod-script-pipe enabled first. On
  windows it asserts provisioning + the honest fail-closed pipe error.
  **RUN by the manager on macos-host (linux/amd64, epoch59), 2026-10-18 —
  PASS twice.** Not run on the authoring host (dev-host) and not run on
  windows-host (windows/amd64 stays provision-only, below). Record:
  - Artifact: the official Audacity 3.7.3 linux amd64 AppImage
    (`audacity-linux-3.7.3-x64-22.04.AppImage`, 108,603,896 bytes),
    sha256-verified against the official `CHECKSUMS.txt` digest that is the
    committed pin.
  - The AppImage's static launcher could not execute under the host's
    amd64 binfmt, so the squashfs payload was extracted and Audacity was
    started from the extracted tree. The extraction was for that reason
    only; the bytes verified are the official AppImage, unmodified.
  - First run honest result: Audacity's first-run modal (the one-time
    welcome/update dialog) blocked the pipe server, so the first `TestLive`
    attempt hit the bounded timeout. The modal was dismissed once in the UI
    and Audacity was restarted (the one-time operator setup this document
    already requires; the wrapper itself enabled and dismissed nothing).
  - Then the real `TestLive` drove production `NewCmd` → `Path` →
    `binmgr` → `defaultPipeClient` over the same-user FIFO pair: **PASS**
    twice. Each run's `Import2` and `GetInfo` replies each ended in
    `BatchCommand finished: OK` and `GetInfo` returned the real Tracks JSON
    for the generated WAV. First PASS 0.40 s (operation 383 ms); repeat
    PASS 0.30 s (operation 289 ms).
  - Cleanup: the exact container used and its 539 MiB temp were removed;
    no host jobs were left behind; macos-host epoch59 released.
- Live pin-verify (darwin/arm64 host, 2026-10-18, no GUI) — SEPARATE from
  full pipe transaction:
  - `ArtifactFor("3.7.3","darwin","arm64")` → false (`ErrNoArtifact`);
    `ArtifactFor("4.0.0",…)` → false; `Path` on darwin → `ErrNoArtifact`
    (correctly fails closed, no network).
  - `Spec("4.0.1")` → `ErrUnsupportedVersion`; `Run("4.0.1",…)` → same
    error before pipe (Audacity 4 correctly rejected, no download).
  - `binmgr.EnsureFor` linux/amd64 with empty `BASHY_BIN_CACHE` → **PASS**:
    downloaded `audacity-linux-3.7.3-x64-22.04.AppImage` (108,603,896 bytes)
    from `https://github.com/audacity/audacity/releases/download/Audacity-3.7.3/`,
    verified sha256 `4e7600c3d9f09dca52c7171cc0bfe76abb168436143cd02ecfa3052ab9f7aaa1`
    against committed pin (hash check passed), chmod +x, reported as
    `…/.targets/linux/amd64/audacity/3.7.3/audacity` with `-rwxr-xr-x`,
    3.52 s wall time. This proves the linux pin is the official
    `CHECKSUMS.txt` digest and that binmgr's hash verification rejects a
    tampered AppImage.
  - `binmgr.EnsureFor` windows/amd64 → **hash PASS, entrypoint check
    fails cross-platform**: the 26,398,933-byte
    `audacity-win-3.7.3-64bit.zip` was downloaded and its sha256 verified
    against the committed pin `1d4c91d0bc1b5376f4c8768d48a6e79ab9dcb4600c1162053f61556becb45ce5`
    (same value `curl …/CHECKSUMS.txt` publishes and the authoring host's
    independent `sha256sum` of the bytes), but extracting the Tree on a
    darwin host leaves `Audacity.exe` without the Unix exec bit, so
    binmgr reports `entrypoint missing` after successful hash verification.
    This is a cross-platform exec-bit artifact (blender's windows pin had
    the same behavior until run on a windows host); the supply-chain pin
    itself is verified. Live execution of `Audacity.exe` on windows/amd64
    still requires a windows host (not done here).
  - Live pipe session on linux/amd64: **RUN** — the `info` operation
    (`Import2` + `GetInfo`) over the real same-user FIFO against the real
    Audacity 3.7.3 is the manager's `TestLive` PASS ×2 on macos-host
    epoch59 recorded above. `trim`/`normalize`/`export` share the same
    transport and framing (fixture-pinned command sets) but were NOT driven
    live in this story; their live run is listed under "What remains".
    On the authoring host (darwin/arm64) `audacity.Run` correctly fails
    closed with `ErrNoArtifact` before dialing any pipe; no pipe
    (`/tmp/audacity_script_pipe.*` / `\\.\pipe\ToSrvPipe`) was dialed
    and no network was used from dev-host. windows/amd64 remains
    provision-only: no live `Audacity.exe` session has been run.
- Registry / atlas / `cmds/all`: NOT touched by this story. `audacity` is
  reachable through `audacity.NewCmd()` (the cobra front door the live
  smoke drove on macos-host); mounting it on the bashy command/MCP surface is a
  separate, reviewed change (atlas census + registry pin gate).
- Go-side dependencies: none added (`go.mod`/`go.sum` untouched; the
  package uses `pkg/binmgr`, cobra, and stdlib only).
- Operation schema/effects: declared in Go via `EffectsFor` and
  `PipeCommands`, consumed by the registered-command contract; no ad-hoc
  schema. Local fixtures only; `YOKE_AUDACITY_LIVE_DIR` live fixtures
  would be isolated (not committed).

## What remains (out of this story's acceptance)

- Live `trim`/`normalize`/`export` sessions on linux/amd64 (only `info`
  was driven live via `TestLive` on macos-host epoch59), and the
  windows/amd64 live run (`YOKE_AUDACITY_LIVE=1 go test -count=1 -run
  TestLive -v ./external/audacity` on windows-host asserts provisioning + the
  fail-closed pipe error; a Windows named-pipe transport is a separate
  story).
- Mounting `audacity` on `cmds/all` / the bashy command + MCP surface
  (requires atlas/registry review).
- Create-to-post integration and any Wave 4 breadth beyond these four ops.
