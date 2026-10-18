# blender evidence — story f5817629a95b (sprint 382, issue #353)

Yoke-side admission and verification record for the Blender external.
Build/license admission, fixture tests, and live verified support are
distinct rows below; a compiling wrapper or a passing fake-executor test
is NOT live support. windows/amd64 is pinned and LIVE-VERIFIED (see
Pins and Live evidence); every other platform still fails closed.

## Admission (reviewed)

- Candidate: Blender — `blender/blender`, GPL-3.0-or-later.
- License posture: download + exec ONLY. Nothing from Blender is linked,
  vendored, embedded, or modified; yoke invokes the unmodified official
  binary as a separate process, which creates no derivative work. The
  fixed bpy programs in `blender.go` are yoke-authored scripts run BY
  Blender, not Blender code.
- Pinned version: `DefaultVersion = "4.2.3"` (4.2 LTS line). No "latest":
  a version change is a reviewed edit of the constant plus matching pins.
- Official artifacts (`ArtifactFor`, `blender.go`), release root
  `https://ftp.blender.org/release/Blender<MAJOR.MINOR>/` — a Blender
  Foundation origin for the same release tree as download.blender.org
  (same paths, bytes and `.sha256` manifest). download.blender.org was
  the original root; it sits behind a Cloudflare managed browser
  challenge that answers plain HTTP clients (binmgr, curl, PowerShell,
  BITS) with an interstitial instead of the archive — run379 of this
  story died on exactly that. ftp.blender.org serves the files directly
  over HTTPS (`TestOfficialSourceAndPin` pins the host):
  - linux/amd64: `blender-<v>-linux-x64.tar.xz`, entrypoint
    `blender-<v>-linux-x64/blender` (Tree asset).
  - windows/amd64: `blender-<v>-windows-x64.zip`, entrypoint
    `blender-<v>-windows-x64/blender.exe` (Tree asset).
  - darwin/arm64, darwin/amd64: NOT DECLARED — the Foundation publishes
    only `.dmg`, which `pkg/binmgr` does not unpack. `ErrNoArtifact`.
  - linux/arm64, windows/arm64: NOT DECLARED — no official archive known
    for 4.2.x. `ErrNoArtifact`.
- Mode: short-lived client only (supply-chain Rule 2). Exactly one
  foreground `blender --background --factory-startup …` child per
  operation, created through `binmgr.Command` and waited on with `Run`
  under the caller's context (plus an optional `--timeout`). No server,
  daemon, listener, detached/background child, MCP server, GUI, or
  resident mode is admitted (`TestNoDaemonListenerOrDetach`,
  `TestRealExecutorHonoursDeadline`).

## Pins (honest status)

- `pkg/binmgr/pins.go` carries ONE entry,
  `blender@4.2.3/windows/amd64 =
  7f8be9b1dc904e7689b3112659791c29cc9ed9d46ce57c4ea5b31a0621489b3a`.
  The value is the Blender Foundation's own published digest for
  `blender-4.2.3-windows-x64.zip`, copied from the official manifest
  `https://ftp.blender.org/release/Blender4.2/blender-4.2.3.sha256`
  (fetched 2026-10-18 from the authoring host; manifest sha256
  `0288570c…583155`, 576 bytes; same value as download.blender.org
  publishes). It was NEVER computed from a mirror or from the bytes
  that happened to arrive: binmgr downloaded the archive on noviwin1
  and verified it AGAINST this pin before extracting (Live evidence).
  `TestOfficialSourceAndPin` pins the constant so a drift is a test
  failure, and the asset declares no SHA512/MD5.
- linux/amd64 is NOT pinned. The same manifest lists
  `3a64efd1982465395abab4259b4091d5c8c56054c7267e9633e4f702a71ea3f4`
  for `blender-4.2.3-linux-x64.tar.xz`; it is recorded here for the
  follow-up that performs the live download-verify-run on a Linux host,
  and until then `Path` fails closed with `ErrNoPin` on linux/amd64 and
  no bytes are fetched. darwin/* and */arm64: `ErrNoArtifact`.
- `Spec` declares an asset only for a platform whose pin exists
  (`TestSpecAndPathFailClosedWithoutCommittedPin`, run against an empty
  `BASHY_BIN_CACHE`): windows/amd64 is "asset declared, Tree,
  Entrypoint set"; every other platform has no asset, no network. The
  release-supplied `.sha256`/`.md5` sidecars are never consulted at
  provision time.

## The four operation boundaries

Every operation takes explicit ABSOLUTE paths (relative paths, control
characters, option-looking values, wrong extensions, missing inputs,
missing output parents are refused BEFORE provisioning or execution —
`TestValidationRunsBeforeExecution`, `TestResolverFailureMeansNoExecution`)
and runs ONE fixed bpy program passed via `--python-expr`, with
`--python-exit-code 3` so a script exception fails the child. Paths reach
the script only after `--`. Each script prints exactly one
`yoke-blender-v1 `-prefixed JSON line; the wrapper filters Blender's
stdout noise and emits that JSON alone (`TestSceneInspectArgv`).

| Operation | Inputs | Output | Fixed script does |
|---|---|---|---|
| `scene-inspect --input X.blend` | existing `.blend` | JSON (`yoke-blender-scene-v1`) | scene, frame range, render settings, active camera, objects (sorted), collections, materials |
| `asset-import --asset A --output O.blend [--input B.blend]` | `A` ∈ obj/fbx/gltf/glb/stl/ply/usd/usda/usdc/usdz; optional base `.blend` | writes `O.blend`; JSON (`yoke-blender-import-v1`) | empty scene (or base), import by extension via the stock importer, save-as |
| `render --input X.blend --output F.png [--frame N]` | existing `.blend`, `N ≥ 0` (default 1) | writes `F.png`; JSON (`yoke-blender-render-v1`, includes `camera`) | frame_set, PNG, `write_still`; no active camera → the first camera object by name becomes active (deterministic); no camera object at all → refused. Never creates objects (`TestRenderScriptPicksADeterministicCameraOnlyWhenNoneIsActive`) |
| `output-inspect --input F.<img>` | png/jpg/jpeg/exr/tif/tiff/bmp/hdr/tga | JSON (`yoke-blender-output-v1`) | `bpy.data.images.load`; size, channels, depth, format, colorspace, alpha |

Exact argv per operation is pinned by `TestSceneInspectArgv`,
`TestAssetImportArgv`, `TestRenderArgv`, `TestOutputInspectArgv`;
`TestEveryArgvIsBackgroundWithAFixedScript` pins `--background` first,
exactly one `--python-expr` carrying one of `Scripts()`, and the absence
of `--python`, `--python-console`, `--python-use-system-env`, `--addons`,
`--command`.

## Unsupported modes (refused, not deferred)

- Free-form Python (`--python`, `--python-expr` from the user,
  `--python-console`), arbitrary Blender arguments, positional
  pass-through (`TestNoFreeFormEscapeFlags`: unknown flags, positionals
  and a trailing `--` all fail, executor never called).
- Any resident process: `blender` GUI, `--python-console` REPL, an MCP
  server, a render farm/daemon, a listener. `blender.go` imports no
  `net*`, `os/signal`, `os/exec`, `syscall`; contains no `.Start(`,
  `binmgr.Launch/Start/RunSpec`, `Setsid/Setpgid`, `Listen(`, `go func(`.
- Animation ranges, non-PNG render formats, add-on installation,
  system-Python environments, cached/user startup (`--factory-startup`
  is always passed).
- Host-installed Blender: there is no `PreferHost` path; only the pinned,
  binmgr-verified artifact is ever executed.

## Verification status

- Fixture tests (this repo, no network, no Blender):
  `gofmt -l external/blender pkg/binmgr` clean; `go vet` of both for
  GOOS=windows/linux/darwin; `go test -count=1 ./external/blender
  ./pkg/binmgr` — PASS on darwin/arm64 (authoring host) AND on
  noviwin1 windows/amd64 (see below). The fake executor never starts a
  process; the two `TestRealExecutor*` tests exercise the real
  `runCommand` with `/bin/sleep` and `/bin/echo` (skipped on Windows).
- Registry / atlas / `cmds/all`: NOT touched by this story. `blender` is
  reachable through `blender.NewCmd()` (the cobra front door the live
  smoke drives); mounting it on the bashy command/MCP surface is a
  separate, reviewed change (atlas census + registry pin gate).
- Go-side dependencies: none added (`go.mod`/`go.sum` untouched; the
  package uses `pkg/binmgr`, cobra, and stdlib only).

## Live evidence — windows/amd64, noviwin1.local, 2026-10-18

Story f5817629a95b, sprint 382, run by the original holder on the
claimed noviwin1 (epoch11). Nothing ran on the authoring host (Dragon)
except fetching the 576-byte manifest. Everything on noviwin1 ran in an
isolated temp tree, not a shared checkout:

- Host: `noviwin1` — Windows NT 10.0.26200, AMD Ryzen 5 7520U with
  Radeon Graphics (integrated), 16 GB; `go1.27.2 windows/amd64`
  (GOTOOLCHAIN=auto from the host's go1.27.0). Session: Windows OpenSSH,
  non-interactive `cmd.exe`.
- Staging: the working tree (not HEAD — it carried this story's edits)
  was tarred on the authoring host (`/usr/bin/tar … --exclude=.git`,
  36,103,680 bytes, sha256 `e5dd3b24…047e5461`, re-hashed with
  `certutil` after `scp`) into
  `%TEMP%\yoke-f581-20261018T071431Z\{src,cache,work}`;
  `BASHY_BIN_CACHE=…\cache` (empty), `YOKE_BLENDER_LIVE=1`,
  `YOKE_BLENDER_LIVE_DIR=…\work`. Preflight: `tasklist /FI "IMAGENAME eq
  blender.exe"` → no tasks.
- Unit tests on Windows (`go test -count=1 ./external/blender
  ./pkg/binmgr -run "TestSpecAndPathFailClosedWithoutCommittedPin|
  TestOfficialSourceAndPin|TestArtifactTable|
  TestEveryArgvIsBackgroundWithAFixedScript|TestNoDaemonListenerOrDetach|
  TestRenderScriptPicks|TestPinnedDigests_ValidFormat|TestEnsure_Pin"`) —
  `ok` both packages; full `go test -count=1 ./external/blender` — `ok`.
- Live smoke (`go test -count=1 -v -timeout 60m -run TestLive
  ./external/blender`, `external/blender/live_test.go`), from an EMPTY
  cache, through the real path `NewCmd → Path → binmgr.Ensure
  (download → sha256 == committed pin → extract) → runCommand`. Each
  operation is one foreground `blender --background --factory-startup …`
  child; `--timeout 15m` bounds the two heavy ones. Fixture: a
  hand-written `cube.usda` (unit cube + camera + distant light), so the
  first `.blend` is produced by `asset-import` itself.

  | Step | Command (abs paths under `…\work`) | Wall time | Result line (abridged) |
  |---|---|---|---|
  | provision | `Path(ctx, "")` → `…\cache\blender\4.2.3\blender-4.2.3-windows-x64\blender.exe` (82,756,224 bytes) | 21.2 s (383,613,057-byte zip from ftp.blender.org, verified against the pin, extracted) | — |
  | asset-import | `blender asset-import --asset cube.usda --output cube.blend --timeout 15m` | 4.8 s | `yoke-blender-import-v1`, `fresh_scene: true`, `imported: ["Camera","Cube","Sun","World"]`, `object_count: 4`; `cube.blend` 412,064 bytes |
  | scene-inspect | `blender scene-inspect --input cube.blend` | 2.6 s | `yoke-blender-scene-v1`, `blender: "4.2.3 LTS"`, `camera: null`, objects CAMERA/MESH/LIGHT/EMPTY, `render.engine: BLENDER_EEVEE_NEXT`, 1920×1080 @100 % |
  | render | `blender render --input cube.blend --output frame.png --frame 1 --timeout 15m` | 6.8 s | `yoke-blender-render-v1`, `camera: "Camera"` (picked by the deterministic fallback — the inspected scene had none active), EEVEE Next, 1920×1080; `frame.png` 1,065,345 bytes, sha256 `720ec6a5…2a8c640a` |
  | output-inspect | `blender output-inspect --input frame.png` | 0.7 s | `yoke-blender-output-v1`, PNG 1920×1080, 4 channels, depth 32, sRGB, `has_data: true` |
  | pixel check | stdlib `image/png` decode in the test | — | 349,418 of 2,073,600 pixels lit (16.85 %); frame fetched to the authoring host and eyeballed: a sun-lit cube on black |

  `--- PASS: TestLive (36.20s)`, `ok github.com/qiangli/yoke/external/blender 37.520s`.
  An earlier identical run WITHOUT the light rendered solid black (every
  step still passed) — that is why the fixture carries a `DistantLight`
  and the test asserts lit pixels. The first run of the day also showed
  the cache hit path: a second `Path` call on the populated cache
  resolved in 0 s with no network.
- Rule-2 contract, observed: one `blender.exe` per step, exits before
  the next; `tasklist` after every run → no `blender.exe`; no daemon,
  listener, MCP server, GUI, or resident process was created.
- Cleanup: `rmdir /s /q %TEMP%\yoke-f581-20261018T071431Z` → removed
  (verified with `if exist`); `%TEMP%` has no `f581`/`blender` entries;
  the host's default binmgr cache (`%LOCALAPPDATA%\bashy\bin`) holds no
  blender; final `tasklist` → no `blender.exe`. On the authoring host
  the staging tar, logs, manifest copy and the two fetched frames were
  deleted; nothing Blender-related was downloaded or run there.
- Not done here (out of this story's remaining acceptance): mounting
  `blender` on `cmds/all` / the bashy command + MCP surface;
  linux/amd64 pin + live run; create-to-post; Wave 4.
