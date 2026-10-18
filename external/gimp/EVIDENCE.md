# gimp evidence — story 95d4747436c8 (sprint 382)

Yoke-side admission and verification record for the GIMP external.
Build/license admission, fixture tests, and live verified support are
distinct rows below; a compiling wrapper or a passing fake-executor test
is NOT live support. linux/amd64 is PINNED with manifest+bytes verified
digests (see Pins); on-target batch execution on Linux is the named gap,
and every other platform fails closed.

Precedent: `external/blender` (story f5817629a95b) is the accepted
verified-provisioning pattern this story follows — managed external,
official artifact plus committed digest pin, one bounded foreground child
per typed operation, fail closed where unsupported/unverified, no resident
mode, fixture/live evidence separated.

## Admission (reviewed)

- Candidate: GIMP — `GNOME/gimp`, GPL-3.0-or-later (COPYING is the GPL v3
  text; confirmed 2026-10-18 from the project's own tree).
- License posture: download + exec ONLY. Nothing from GIMP is linked,
  vendored, embedded, or modified; yoke invokes the unmodified official
  binary as a separate process, which creates no derivative work. The
  fixed Python-Fu programs in `gimp.go` are yoke-authored scripts run BY
  GIMP, not GIMP code. Same posture as the admitted Blender external.
- Pinned version: `DefaultVersion = "3.2.6"` (3.2 line; current stable per
  the project's `0.0_LATEST-IS-3.2.6` marker, 2026-09-10). No "latest": a
  version change is a reviewed edit of the constant plus matching pins.
- Official artifacts (`ArtifactFor`, `gimp.go`), release root
  `https://download.gimp.org/gimp/v<MAJOR.MINOR>/` — the project's own
  download service. It answers plain HTTPS clients with a 302 into its
  mirror network (observed 2026-10-18: `linux/SHA256SUMS` resolved to
  `mirrors.ocf.berkeley.edu`); binmgr follows the redirect and every byte
  is verified against the COMMITTED pin, so the serving mirror is never
  the trust anchor.
  - linux/amd64: `GIMP-<v>-x86_64.AppImage` (raw binary: download →
    verify → chmod +x → exec; no Tree, no entrypoint).
  - linux/arm64: `GIMP-<v>-aarch64.AppImage` (official; declared by
    `ArtifactFor` but UNPINNED — `ErrNoPin`, no network).
  - windows/amd64+arm64: NOT DECLARED — the project publishes only an
    installer `.exe`, which binmgr does not unpack or execute.
    `ErrNoArtifact`.
  - darwin/arm64, darwin/amd64: NOT DECLARED — the project publishes
    only `.dmg`, which `pkg/binmgr` does not unpack. `ErrNoArtifact`.
- Mode: short-lived batch client only (supply-chain Rule 2). Exactly one
  foreground batch child per operation
  (`--no-interface --no-data --no-fonts --new-instance
  --batch-interpreter python-fu-eval --batch <fixed> --quit`), created
  through `binmgr.Command` and waited on with `Run` under the caller's
  context (plus an optional `--timeout`). No GUI, no server, daemon,
  listener, detached/background child, MCP server, plug-in installation,
  or resident mode is admitted (`TestNoDaemonListenerOrDetach`,
  `TestRealExecutorHonoursDeadline`).
- gimp-mcp (`maorcc/gimp-mcp`) is REFERENCE ONLY. Its architecture — a
  plug-in installed into GIMP's plug-ins directory (GIMP restart
  required) plus a separate Python MCP server speaking to GIMP over a
  resident bridge, with 56 tools and a live snapshot channel — is
  explicitly NOT admitted: no plug-in is installed, no bridge is opened,
  no host GIMP is ever located or used (no `LookPath`, no `PreferHost`,
  no `GIMP_MCP_BIN`/`GIMP_CONSOLE` override — pinned by test), and the
  operation count is three fixed batch programs, not 56 live tools. What
  was taken from the reference ecosystem is the GIMP 3 batch SHAPE only:
  `python-fu-eval` as the 3.x batch interpreter, `--quit` to exit after
  batch, and the `Gimp.file_load / image.scale / Gimp.file_save /
  image.delete` call family.

## Pins (honest status)

- `pkg/binmgr/pins.go` carries ONE entry,
  `gimp@3.2.6/linux/amd64 =
  79ea41bc9b06f78fda181849a9ca8e42d83f1124dedb756f4d52352e2465010f`.
  The value is the GIMP project's own published digest row for
  `GIMP-3.2.6-x86_64.AppImage` in
  `https://download.gimp.org/gimp/v3.2/linux/SHA256SUMS` (fetched
  2026-10-18 from the authoring host; 1333 bytes, 14 rows). It was then
  matched against the bytes that actually arrived: the full
  243,476,984-byte AppImage was downloaded on the authoring host and
  `shasum -a 256` reproduced the manifest value exactly (manifest AND
  bytes, never a digest computed from a mirror alone).
  `TestOfficialSourceAndPin` pins the constant so a drift is a test
  failure, and the asset declares no SHA512/MD5 and no archive fields.
- linux/arm64 is NOT pinned. The same manifest lists
  `d1b8271e440dc4e36065ca25f219c0534abd2dfb43aeb033c76b16832d395e19`
  for `GIMP-3.2.6-aarch64.AppImage`; it is recorded here for the
  follow-up that performs the live download-verify-run on an arm64 Linux
  host, and until then `Path` fails closed with `ErrNoPin` on
  linux/arm64 and no bytes are fetched. windows/* and darwin/*:
  `ErrNoArtifact`.
- `Spec` declares an asset only for a platform whose pin exists
  (`TestSpecAndPathFailClosedWithoutCommittedPin`, run against an empty
  `BASHY_BIN_CACHE`): linux/amd64 is "asset declared, raw binary"; every
  other platform has no asset, no network. The release-supplied
  `SHA256SUMS`/`SHA512SUMS` sidecars are never consulted at provision
  time.

## The three operation boundaries

Every operation takes explicit ABSOLUTE paths (relative paths, control
characters, option-looking values, wrong extensions, missing inputs,
missing output parents are refused BEFORE provisioning or execution —
`TestValidationRunsBeforeExecution`, `TestResolverFailureMeansNoExecution`),
runs ONE fixed Python-Fu program via a single `--batch`, and exits via
`--quit`. Paths travel only inside escaped string literals in that
program (never as argv); scale integers are range-checked digits and
mode/rotate travel as closed-vocabulary call fragments. A hostile path
(`o'b\x $HOME \`id\``) parses as inert string data under the system
python3 syntax oracle (`TestProgramsParseAsOneFixedStatementShape`).
Success requires exit 0 AND the marked result line AND a non-empty output
file; the wrapper appends the observed `output_bytes`.

| Operation | Inputs | Output | Fixed program does |
|---|---|---|---|
| `export --input I --output O` | existing image (xcf/png/jpg/jpeg/tif/tiff/bmp/gif/webp) | writes flat `O` (png/jpg/jpeg/tif/tiff/bmp/webp); JSON (`yoke-gimp-export-v1`) | `Gimp.file_load` → `Gimp.file_save` (format follows `O`) |
| `convert --input I --output O --mode rgb\|gray` | as above + closed mode enum | writes flat `O`; JSON (`yoke-gimp-convert-v1`) | load → `convert_rgb()` / `convert_grayscale()` → save |
| `edit --input I --output O [--scale WxH] [--rotate 90\|180\|270]` | as above + ≥1 of scale (sides 1..16384) / quarter-turn | writes flat `O`; JSON (`yoke-gimp-edit-v1`) | load → `scale(w, h)` and/or `rotate(RotationType)` → save |

Exact argv per operation is pinned by `TestArgvShape` / goldens in
`TestFixedProgramsCarryOnlyFixedCalls`;
`TestEveryArgvIsBatchWithAFixedProgram` pins exactly one `--batch`
carrying a fixed-template program, exactly one `--batch-interpreter
python-fu-eval`, trailing `--quit`, and the absence of GUI/batch-escape
flags.

Program-body provenance (doc-verified, execution-unverified): every
call in the fixed template was checked against the project's own
`gimp-api-docs-3.2.6` (`v3.2/api-docs/gimp-api-docs-3.2.6.tar.xz`,
fetched 2026-10-18) — `Gimp.file_load(run_mode, file)`,
`image.scale(new_width, new_height)`,
`image.convert_rgb()` / `image.convert_grayscale()` (no args),
`Gimp.file_save(run_mode, image, file, None)` ("saves to XCF or export
image to any supported format by extension", options NULL),
`image.delete()`, `Gimp.RunMode.NONINTERACTIVE`, and
`GimpRotationType` members `GIMP_ROTATE_DEGREES90/180/270` (this check
caught and fixed a wrong `ROTATE_90`-style spelling before any live
run). What the docs do NOT pin down is the turn direction of the
rotation members and runtime behavior under `--quit`, which the live run
below confirms. Validation, argv, escaping, provisioning and refusals do
not depend on those spellings.

## Operation schema/effects (registered-command declaration, unmounted)

`Operations()` declares each operation's input schema and author-declared
security effects in the atlas vocabulary (`exec, read, write` — file
reads, one file write, one spawned child; no net at operation time, no
cred/priv/remote/persist/spend/destroy), and `Registered()` declares the
command-level record (`download`, `fileutils`, `userland`, `cross`,
`json`, effects = the sorted union, OS = `[linux/amd64]`),
`TestRegisteredDeclarationMatchesContract` pins every field against the
`pkg/atlas` closed vocabularies. Registry / atlas / `cmds/all`: NOT
touched by this story — mounting the declaration on the bashy
command/MCP surface is a separate, reviewed change (atlas census +
registry pin gate), exactly as the Blender story left its own mounting
out.

## Unsupported modes (refused, not deferred)

- Free-form programs (`--batch`/`--batch-interpreter` from the user,
  `exec(open(…))` of a script file, positional pass-through) and
  arbitrary GIMP arguments (`TestNoFreeFormEscapeFlags`: unknown flags,
  positionals and a trailing `--` all fail, executor never called).
- Any resident process: the GIMP GUI, a plug-in host, an MCP server, a
  snapshot listener, a daemon. `gimp.go` imports no `net*`,
  `os/signal`, `os/exec`, `syscall`; contains no `.Start(`,
  `binmgr.Launch/Start/RunSpec`, `Setsid/Setpgid`, `Listen(`, `http.`,
  `go func(`, `LookPath`, `PreferHost`, `exec(open`.
- Plug-in installation (nothing is written to any GIMP plug-ins
  directory), host-installed GIMP (no `PreferHost` path; only the pinned,
  binmgr-verified AppImage is ever executed), XCF outputs (flat formats
  only — no layer-fidelity promise is made), GUI flags.
- The gimp-mcp resident bridge (plug-in + Python MCP server + live
  snapshot channel): reference only, not admitted, not implemented.

## Verification status

- Fixture tests (this repo, no network, no GIMP):
  `gofmt -l external/gimp pkg/binmgr` clean; `go vet` of both for
  GOOS=windows/linux/darwin; `go test -count=1 ./external/gimp
  ./pkg/binmgr` — PASS on darwin/arm64 (authoring host). The fake
  executor never starts a process; the two `TestRealExecutor*` tests
  exercise the real `runCommand` with `/bin/sleep` and `/bin/echo`
  (skipped on Windows); the python3 syntax oracle parses every fixed
  program including a hostile-path case (skipped only where no python3
  exists — present on the authoring host, 3.9.6).
- `live_test.go` (`TestLive`, `YOKE_GIMP_LIVE=1`) is compile-verified
  here (vet builds it) and execution-unverified: it provisions through
  the real path and runs export/convert/edit over a stdlib-generated
  PNG, asserting result schemas and exact scaled dimensions.
- Go-side dependencies: none added (`go.mod`/`go.sum` untouched; the
  package uses `pkg/binmgr`, cobra, and stdlib only; `pkg/atlas` is a
  test-only import for vocabulary pins).

## Live evidence — 2026-10-18, authoring host (darwin/arm64)

Digest + bytes verification (no GIMP executed — no provisionable macOS
artifact exists, so nothing ran here):

- Manifest: `curl -sSL
  https://download.gimp.org/gimp/v3.2/linux/SHA256SUMS` → HTTP 200 via
  302 to `mirrors.ocf.berkeley.edu`, 1333 bytes; row
  `79ea41bc9b06f78fda181849a9ca8e42d83f1124dedb756f4d52352e2465010f
  GIMP-3.2.6-x86_64.AppImage` (plus the aarch64 row recorded under Pins).
- Bytes: full download of `GIMP-3.2.6-x86_64.AppImage` (243,476,984
  bytes) → `shasum -a 256` =
  `79ea41bc9b06f78fda181849a9ca8e42d83f1124dedb756f4d52352e2465010f`
  — exact match with the manifest row. Staging bytes deleted afterwards;
  nothing GIMP-related was installed or executed on the authoring host.
- License: `GNOME/gimp` COPYING fetched from the project's tree — GNU
  GPL version 3 (copyleft); posture is download+exec only, as recorded
  under Admission.
- Re-verified on resume (2026-10-18, same host, after the first run was
  killed): `linux/SHA256SUMS` fetched again (HTTP 200 via 302 to
  `mirrors.ocf.berkeley.edu`, 1333 bytes, 14 rows); row 13 is still
  `79ea41bc…010f  GIMP-3.2.6-x86_64.AppImage`, equal to the committed
  pin. The `windows/` and `macos/` listings for 3.2.6 carry only
  `gimp-3.2.6-setup.exe` and `gimp-3.2.6-{arm64,x86_64}.dmg`, so the
  fail-closed rows above still hold.
- Conductor STOP (Dragon policy, 2026-10-18): no Linux container was
  booted and no GIMP was executed on the authoring host; only the
  fixture-only suite ran here. The on-target live run is owned by the
  manager on an approved host after code review.

## Named gaps (not done here)

- On-target live run: `YOKE_GIMP_LIVE=1` provision + export/convert/edit
  on a linux/amd64 host (first run confirms/adjusts the fixed program
  bodies, AppImage FUSE behavior in the target environment, and
  `--quit` semantics), then the linux/arm64 pin + run.
- Mounting `Registered()` on `cmds/all` / registry / atlas and the bashy
  command + MCP surface: separate, reviewed change.
- Wave 4 follow-ups per the story's sprint card.

## Dragon policy-breach cleanup — 2026-10-18 (authoring host "Dragon", darwin/arm64)

An earlier worker on this story had begun preparing a Lima Linux VM
(`gimp-live`) on Dragon to run GIMP live. That is forbidden by Dragon
policy (no podman/container/VM/native heavy creative runs). Cleanup,
supervisor-authorized, at 05:50 PDT:

- `limactl stop -f gimp-live` then `limactl delete gimp-live` — the VM
  instance was force-stopped and deleted; no output of a GIMP run
  existed to preserve, because GIMP never started inside it.
- Dependency + mtime check before uninstalling: both `lima` 2.2.1 and
  `lima-additional-guestagents` 2.2.1 had been installed by brew tonight
  (the same session) and `brew uses --installed` showed no other
  dependents of either. `lima-additional-guestagents` was uninstalled
  first, then `lima`. No shared or preexisting package was removed.
- Post-cleanup state, re-verified at 05:58 PDT while writing this
  section: `limactl` is absent from PATH, `~/.lima/gimp-live` does not
  exist, `brew list --formula` shows no lima formula, and `df -h /`
  reports 47 GiB free on the data volume (about 50 GiB immediately after
  the deletion, per the supervisor's reading).
- Live GIMP was NOT executed on Dragon at any point — not in the VM,
  not in a container, not natively. The only runs here were the
  fixture-only suite recorded under Verification status. Live evidence
  remains manager-gated to a separately claimed approved host
  (novidesign or noviwin).
- No other source, test, or pin changes accompany this section.
