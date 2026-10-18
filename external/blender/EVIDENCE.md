# blender evidence — story f5817629a95b (sprint 382, issue #353)

Yoke-side admission and verification record for the Blender external.
Build/license admission, fixture tests, and live verified support are
distinct rows below; a compiling wrapper or a passing fake-executor test
is NOT live support, and NO platform is provisionable today (see Pins).

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
  `https://download.blender.org/release/Blender<MAJOR.MINOR>/`:
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

- `pkg/binmgr/pins.go` contains NO `blender@4.2.3/<platform>` entry.
  Verified official SHA256 values were not already known to this story
  and were NOT fetched: no Blender artifact was downloaded on the
  authoring host and no hash was invented.
- Consequence: `Spec` declares an asset only for a platform whose pin
  exists, so today `Spec(DefaultVersion).Assets` is EMPTY and `Path`
  fails closed on every platform — `ErrNoPin` where an official archive
  exists (linux/amd64, windows/amd64), `ErrNoArtifact` elsewhere — with
  no network access (`TestSpecAndPathFailClosedWithoutCommittedPin`,
  run against an empty `BASHY_BIN_CACHE`). The release-supplied
  `.sha256`/`.md5` sidecars are never consulted.
- To enable a platform: on a trusted host download the official archive,
  confirm its sha256 against the Foundation's published checksums out of
  band, and commit `"blender@" + "4.2.3" + "/linux/amd64": "<hex>"` (etc.)
  to `pkg/binmgr/pins.go` through review. The test above then flips that
  platform to "asset declared, Tree, Entrypoint set" automatically.

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
| `render --input X.blend --output F.png [--frame N]` | existing `.blend`, `N ≥ 0` (default 1) | writes `F.png`; JSON (`yoke-blender-render-v1`) | frame_set, PNG, `write_still`; refuses a scene with no active camera |
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
  `gofmt -l external/blender` clean; `go vet ./external/blender`;
  `go test -count=1 ./external/blender` — PASS (see commit for exit
  codes). The fake executor never starts a process; the two
  `TestRealExecutor*` tests exercise the real `runCommand` with
  `/bin/sleep` and `/bin/echo` (skipped on Windows) to prove the
  deadline kills a real child and output is captured in the foreground.
- LIVE BLENDER CHECKS — NOT PERFORMED: no pin exists, so no Blender was
  provisioned and none of the four fixed bpy programs has been run
  against a real Blender 4.2.3 here. The scripts use stable, documented
  4.2 operators (`wm.obj_import`, `import_scene.fbx`,
  `import_scene.gltf`, `wm.stl_import`, `wm.ply_import`,
  `wm.usd_import`, `render.render`, `images.load`); their first live run
  belongs to the story that lands the pins. No live support is claimed.
- Registry / atlas / `cmds/all`: NOT touched by this story. `blender` is
  reachable only through `blender.NewCmd()`; mounting it is a separate,
  reviewed change (atlas census + registry pin gate), deliberately not
  done while no platform is provisionable.
- Go-side dependencies: none added (`go.mod`/`go.sum` untouched; the
  package uses `pkg/binmgr`, cobra, and stdlib only).
