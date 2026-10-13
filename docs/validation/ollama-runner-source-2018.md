# Official Ollama runner source qualification

Sprint 436 · Story 2018 (`fe7ef21ca333`) · parent `732cbb22` · 2026-10-13.
Yoke inspected at `73d10206aa2d4df1caa5a1aa8b5a1b34158eb1af`.

## Decision

Use **official `ollama/ollama` stable `v0.21.2`**, exact commit
**`590109c8352e8d5a6206e8909b518a54a2b0a7b8`**, as the candidate for the
separate runner's hosted build qualification. It contains upstream's own
`cmd/runner/main.go`; no replacement entrypoint or fork patch is necessary.
The llama runner and its launch/load/health protocol match the embedded server's
upstream base. All five requested OS/architecture dependency graphs resolve.

This is **source qualification, not a claim of five successful native builds or
permission to publish**. The native compiler/linker, runtime, vulnerability,
complete compiled-input license, no-egress, SBOM and signing gates remain open.
There are concrete integration blockers below. Keep managed inference disabled
until those gates pass; do not publish the draft from `43768325` or relabel it as
an official tag. Rule 1's stable-tag and security-only-patch requirements stand.

## Provenance and delta

- [Official release](https://github.com/ollama/ollama/releases/tag/v0.21.2):
  GitHub API reports `draft=false`, `prerelease=false`, published
  `2026-04-23T02:29:24Z`. `git ls-remote --tags https://github.com/ollama/ollama.git`
  resolves the exact tag to the commit above. Release assets list Linux amd64,
  Linux arm64, Darwin, Windows amd64 and Windows arm64. No asset was executed.
- Current fork `43768325f` descends from official upstream
  `4656a07e5639a1edb1aaae5ea2c7dcb4021846f3`. `v0.21.2` is its newest numerically
  versioned stable-tag ancestor in the inspected official refs. There are **four
  commits** between the tag and that base. Their 16-file delta concerns API think
  values, OpenAI Responses, command/server request handling, and MLX sampling;
  it does not change `cmd/runner`, `runner`, `ml`, `llama`, or `llm`.
- Fork delta from the base: embedding exports; cgo/no-cgo llama shims and server
  call-site substitutions; Go/toolchain/dependency changes; header relocation
  `llama.cpp/vendor` to `third_party`; MLX closure cgo.Handle fix; build/test
  adjustments. These are **not** an official release. The full fork cannot be
  used as the runner source under Rule 1 merely because it has an entrypoint.
- [v0.40.3](https://github.com/ollama/ollama/releases/tag/v0.40.3) resolves to
  `eab97e9f92b9a25c2d52d2cc6c1b1c99bd9fae21`, stable, published
  `2026-10-11T01:39:04Z`. It is unsuitable as a drop-in: upstream commit
  `9db4bdbad6a4981ad761aa2b603e69e8fb83212c` removed the cgo runners in favour
  of llama-server; `6a0ee480804d09c4d2aabddc71d4901f6e60b36c` removed the
  dispatcher entrypoint. Its `runner/llamarunner` and `runner/runner.go` are
  absent, with a substantially different `llm/llama_server.go` integration.
  Adding a custom main file would neither restore this protocol nor satisfy
  the no-diverging-patches rule. `v0.40.4-rc1` is a prerelease and excluded.

## Protocol evidence

The source gate compares the tag against the exact current fork, not just
similarly named functions. `cmd/runner`, the **entire `runner` tree**, and the
**entire `ml` tree** are byte-identical. `llm/server.go`'s `StartRunner`,
`LoadOperation`/`LoadRequest`/`LoadResponse`, and server-status declarations are
also byte-identical. The remaining server diff replaces local llama calls with
cgo/no-cgo helpers; it does not change HTTP endpoint paths or the wire structs.

`cmd/runner/main.go` calls `runner.Execute(os.Args[1:])`. The dispatcher consumes
an optional leading `runner`, accepts `--ollama-engine`, `--imagegen-engine`,
and `--mlx-engine`, and otherwise calls `llamarunner.Execute`. The current server
launches `runner [--ollama-engine] --model PATH --port PORT`. The llama runner
binds `127.0.0.1`, exposes `/health`, `POST /load`, `/completion`, `/embedding`,
and accepts the same numeric fit/alloc/commit/close operations. Fit/alloc are
successful no-ops in the llama backend; commit loads the weights asynchronously.
Completion streaming and embedding handlers are identical because the entire
runner tree is identical. Native model execution remains untested here.

## Implementation path without upstream patches

1. Give the runner an independent source pin in Bashy build metadata. Fetch the
   official stable tag into the public mirror and verify its peeled SHA against
   the immutable pin before building. Use a full Git source checkout/archive,
   **not a Go module zip**: the latter omits nested `vendor` headers and motivated
   the fork's relocation patch. Leave Yoke's server gitlink alone during this
   runner-source change; replacing it wholesale loses `embed` and the shims.
2. In the tag checkout, use `GOWORK=off`, a pinned supported Go toolchain
   (`go.mod` minimum is 1.24.1), `CGO_ENABLED=1`, and native C/C++ compilers.
   Build upstream `./cmd/runner` directly, e.g.
   `go build -mod=readonly -trimpath -o STAGING/ycode-runner ./cmd/runner`.
   Windows output is `ycode-runner.exe`. Do not generate a custom main, delete
   dispatcher imports, or transplant the `third_party` patch.
3. Initially qualify built-in CPU on all five targets and built-in Metal on
   Darwin arm64. The cgo graph already compiles CPU/llamafile; CMake shared GPU
   libraries are not required by this baseline. For Darwin arm64 generate the
   embedded shader using upstream's directive:
   `go generate ./ml/backend/ggml/ggml/src/ggml-metal`.
   Check all generated inputs in the source inventory. MLX C wrappers are
   already present in the tag; indiscriminate `go generate ./x/...` is unnecessary.
4. Package the exact gated executable, deterministic gzip, and notices. If
   accelerated shared libraries are added later, build them from reviewed source,
   inventory/sign every library and shader, and extend extraction to a verified
   bundle. Today's one-file embed cannot claim to ship CUDA/ROCm/Vulkan/MLX.
   Do not use upstream release binaries as a shortcut.
5. Run native hosted compiler/linker and inference tests for each row below,
   then no-egress, dependency vulnerability audit, compiled-input license review,
   SBOM, exact-workflow Sigstore signing, and artifact verification. Test health,
   load, streaming completion, embedding, cancellation and process shutdown
   through the current embedded server, with a small licensed GGUF fixture.
   Preserve the existing fail-closed managed-service behavior until this passes.

| Target | Source-level path | Native qualification needed |
|---|---|---|
| Linux amd64 | cgo C11/C++17; built-in CPU/llamafile; `-lrt -lpthread -ldl -lstdc++ -lm` | Native GCC/Clang, portable CPU ISA, glibc baseline, ELF dependency inventory |
| Linux arm64 | Same, plus arm64 CPU source selected by build tags | Native arm64 compiler and runtime; no inferred x86 success |
| Darwin amd64 | Built-in CPU; upstream Darwin script explicitly builds amd64 | Xcode/Clang, macOS 14 deployment baseline used by upstream; do not promise Metal acceleration |
| Darwin arm64 | CPU/Accelerate plus `ggml_darwin_arm64.go` importing Metal/BLAS | Generate Metal source, native Apple SDK compilation and actual Metal inference |
| Windows amd64 | Windows cgo flags and static GCC/C++ runtime flags in GGML | Native MinGW-compatible GCC/C++/pthread toolchain, PE/DLL inventory, `.exe`, shutdown and extraction tests |

The official Darwin script builds both architectures; Linux Docker/build scripts
describe both architectures; Windows script has an amd64 branch. These establish
upstream platform intent, not successful builds in this review. No script was run.
The current Yoke `build-runner-thin.sh` only permits three targets and builds the
fork gitlink: it must be replaced/wired to this separate stable source recipe.

## License/dependency inspection and limits

The candidate's root, vendored llama.cpp and GGML licenses are MIT. Reviewed
vendor headers include nlohmann/json (MIT, with embedded Apache attribution),
stb_image (MIT option), and miniaudio (MIT-0 option); llamafile `sgemm.cpp` carries
an MIT-style grant despite the Mozilla copyright. No submodule gitlinks occur
in this tag. Preserve those notices and update distribution attribution when
the runner is actually added.

`go list -mod=readonly -deps -json ./cmd/runner` selects **18 external Go modules**
across the five targets. Their versions and root-license SHA256s are recorded in
the companion JSON. Root licenses inspected: BSD-2/3-Clause for generic-list,
gods, uuid, pflag and Go x/*; MIT for jsonparser, regexp2, easyjson, runewidth,
uniseg; Apache-2.0 for mousetrap, cobra, ordered-map; yaml.v3 includes MIT and
Apache terms. A dependency's prerelease version (gods/v2) does not turn Ollama's
official stable tag into a prerelease, but remains part of the dependency audit.

The dispatcher imports **all four engines**, not only llama. In particular it
compiles MLX C wrapper/header inputs even when no MLX library ships. The MLX
loader records a missing-library error rather than aborting at initialization;
this supports a CPU/Metal baseline, but hosted tests must prove it. Review the
Apple header attribution and generated wrappers in the compiled-input audit;
the root MIT license and this Go-module inventory alone are not full license
clearance. CMake's optional MLX engine is off by default; enabling it introduces
FetchContent inputs and extra libraries which need separate pin/license review.
No OSV/Grype result, native transitive-link inventory, or full source-license
clearance is claimed here.

## Concrete blockers beyond choosing a tag

- **Pure-Go compatibility mode is broken before runner startup.** In current
  `llm/server.go`, when the Ollama tokenizer is unavailable, `loadLlamaModel`
  failure is returned immediately. `llm/llama_nocgo.go` always returns that error;
  its comment claiming a fallback does not match the caller. JSON-schema grammar
  conversion also returns nil without cgo. A runner binary alone cannot repair
  this. Choose a supported Ollama-tokenizer model for a bounded initial proof,
  or implement/review a first-party server adapter, or build the server with cgo.
  Do not silently claim all llama.cpp models now work or add a non-security
  patch to the stable runner. The server fork's existing non-security divergence
  also needs its own Rule 1 resolution; this runner review does not admit it.
- **Dispatch/extraction is a parent integration gate.** The server re-execs
  `os.Executable()` with `runner` arguments, rather than executing the compressed
  payload directly. Within this Yoke tree, `EnsureRunner` has only test callers.
  Verify/add the Bashy executable's early runner dispatch to the signed extracted
  executable, preserving arguments, exit status and termination behavior.
- **Cache integrity is insufficient for Rule 1.** `EnsureRunner` currently trusts
  a hash sidecar of the compressed payload plus existence of the extracted file;
  it does not hash cached executable bytes. It also runs ad-hoc codesign on macOS.
  Verify the actual executable against authenticated release metadata and use
  final CI-signed bytes; do not treat a writable sidecar or ad-hoc signature as
  Sigstore verification. Shared backend discovery must be constrained to verified
  libraries so environment/default paths cannot load unreviewed binaries.

## Gate run and handoff

Ran `python3 scripts/check-ollama-runner-source.py`; inspected the resulting JSON,
not merely the process exit code. Verdict:
`PASS_SOURCE_ONLY_NOT_RELEASE_ADMISSION`. Package/cgo-package counts:
Linux amd64 **344/15**, Linux arm64 **343/15**, Darwin amd64 **344/14**,
Darwin arm64 **345/16**, Windows amd64 **347/14**. No incomplete/error dependency
nodes. Official release metadata/pin, ancestry, identical source trees and
protocol sections passed. Evidence: `ollama-runner-source-2018.json`.

Re-run from the Yoke root; the script uses temporary full-source extraction and
does no generation, native build, model execution, container work or full suite.
It needs the pinned objects, GitHub network access and Go modules. It fails if
the official pin or compared source changes. Parent 732cbb22 remains open for
implementation, native proof and all Rule 1 release gates.
