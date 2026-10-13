# Official Ollama server path for managed inference

Sprint 436 · Story 2021 (`a2945711ef87`) · review of isolated Yoke
`516935de` / `qiangli/ollama` `8c117eac0`.

## Decision

**Do not integrate worker #237 as delivered.** It adds non-security changes to
`llm/llama_cgo.go`, `llm/llama_nocgo.go`, and `llm/server.go` in the third-party
server fork. The Yoke commit pins that fork commit. Rule 1 in the umbrella
`docs/bashy-service-supply-chain-policy.md` permits a server built from a pinned
official stable tag with no diverging patches except security fixes. Focused
`llm` and runner-extraction tests do not qualify a modified server or prove
whole-package no-cgo build or inference. The worker's zero-tensor GGUF and
helper child prove an argument/health boundary only.

**Candidate:** have Bashy CI build and sign the *complete, unpatched* official
Ollama `v0.21.2` executable from peeled source commit
`590109c8352e8d5a6206e8909b518a54a2b0a7b8`. Yoke manages that exact
executable as a child process through first-party adapter code under
`external/ollama/` and talks to its localhost HTTP API. The official executable
implements both `serve` and the hidden `runner` subcommand. Its server already
re-executes `os.Executable()` with `runner [--ollama-engine] --model PATH --port
PORT`; therefore a verified official server executable runs its own official
runner, with no fork-side tokenizer selection and no Bashy-to-runner dispatch
needed on this path. The executable requires cgo/native libraries at **CI
build** time, but Yoke can remain a pure-Go parent process. This is a concrete
source/API contract, **not a successful native build or release admission**.

## Source and module comparison

| Boundary | Worker #237 / current tree | Official-server candidate |
|---|---|---|
| Server source | Yoke embeds fork `github.com/ollama/ollama/server`; worker pins `8c117eac0` | Separate official `v0.21.2` source checkout; `go build` of root `.` (`main.go` → `cmd.NewCLI`) in hosted CI |
| Runner | Bashy separately signs `./cmd/runner`; Yoke re-exec dispatch extracts it | Root executable's `cmd.NewCLI` has `serve` and hidden `runner`, which calls `runner.Execute`; same signed executable handles both |
| Model selection | Fork `llm/server.go` tries Ollama tokenizer only when `OLLAMA_NEW_ENGINE` or model requires it; no-cgo `loadLlamaModel` fails otherwise | Official server uses cgo `llama.LoadModelFromFile` for compatibility models, or official tokenizer when selected; its cgo runner follows the existing protocol |
| Yoke Go dependency | `external/ollama/ollama_component.go` imports fork `server` and `envconfig`; `pkg/ollm` imports fork-only `embed` | First-party process manager and HTTP client; use official stable `api` types if needed, or local wire structs. The fork-only `embed` import must go |

The official tag object is present locally. Source inspection shows root
`main.go` calling `cmd.NewCLI`; `cmd/cmd.go` binds `serve` through
`server.Serve`, and its hidden `runner` command calls `runner.Execute`. The
tag's `llm/server.go` calls `llama.LoadModelFromFile` when the new tokenizer is
not selected. Its `go.mod` requires Go 1.24.1. The source-only qualification
in `ollama-runner-source-2018.md` verifies tag identity and five-target runner
dependency graphs; it does **not** verify a complete server build. The current
fork's additional `embed` package does not exist at the official tag. Its
`NewClient` is only a thin `url.Parse` + `api.NewClient` wrapper; the official
`api/client.go` exports `NewClient`, `List`, `Pull`, `Create`, and `Delete` with
the same request/response types used by `pkg/ollm` at this boundary. These
signatures need a focused compile/API compatibility gate after conversion.

## First-party adapter and signed-artifact contract

1. Add a separate Bashy CI recipe for the official root executable (`.`),
   pinned to the **tag and peeled commit**, using its full Git checkout,
   `GOWORK=off`, `-mod=readonly`, the supported pinned Go/native toolchain,
   and no source patch. Preserve the existing runner-only recipe as a separate
   artifact until this path is accepted. Inventory all compiled Go, C/C++,
   shader, and linked-library inputs for each target. A signed standalone
   `cmd/runner` asset cannot be substituted for the official server binary:
   the official server re-executes its own executable.
2. Produce one complete server executable per target, plus any required
   signed libraries and notices. Apply Rule 1's dependency vulnerability audit,
   compiled-input license review, SBOM, no-egress proof, exact GitHub workflow
   OIDC/Sigstore signature, and per-executable SHA256. Yoke must verify the
   workflow identity, issuer, bundle and executable hash on extraction **and
   every cache use** before starting it. Use an immutable, private cache path;
   reject missing metadata, unexpected files, and unverified library paths.
   The existing `runner_embed` metadata is intentionally empty and cannot
   authorize this different server artifact.
3. In Yoke `external/ollama`, replace the `ollamaserver.Serve` in-process path
   with a supervised `exec.Cmd(verifiedPath, "serve")`. Give the child an
   explicit environment: `OLLAMA_HOST=127.0.0.1:<managed port>`,
   `OLLAMA_MODELS=<managed data dir>/models`, `OLLAMA_NO_CLOUD=1`, restricted
   home/data directories, and Bashy's localhost OTLP endpoint. Reject a door
   URL or user `OLLAMA_HOST` as a server bind. Do not resolve `OLLAMA_BIN`, PATH,
   upstream release assets, or an existing host daemon. Set process lifetime,
   signal forwarding, wait/reap, bounded shutdown and startup health checks
   around the child. Retain `OllamaComponent`'s public `Start`/`Stop`/`Healthy`,
   `Port`/`BaseURL`, HTTP proxy and hook contract for fleet consumers.
4. Move `pkg/ollm` from the fork-only `embed` package to official `api` client
   types or first-party HTTP wire structs. Remove all Yoke imports of fork
   `server`, `llm`, `discover`, and `ml` from its build graph. Pin any remaining
   official API module to the same reviewed tag; do not compile the modified
   fork into the managed service. Keep the existing managed CLI's HTTP verbs
   pointed at the private service URL. Fail closed when the signed server is
   unavailable, preserving the current no-host-fallback behavior.
5. Hosted gate: compile the complete official server and run the signed bytes
   on each supported target; check `/api/version`, model import/load, streaming
   completion and embedding with licensed small GGUF models, cancellation,
   shutdown/reaping, model-store isolation, and no outbound traffic beyond
   user-configured endpoints. A zero-tensor fixture is insufficient. Then
   compile/test Yoke with `CGO_ENABLED=0` against the adapter and verify the
   actual dependency graph no longer reaches the fork's native server packages.

## Current blocker and scope

The official complete server has **not** been built or signed in Bashy CI,
and native server inference has not been proven. Yoke's current component still
imports the fork server, so its full no-cgo package build remains blocked by
the existing MLX/discovery symbols. Worker #237's tests do not remove that
dependency or satisfy Rule 1. Managed local inference must remain fail-closed
until the official server artifact and adapter pass the gates above. No Dragon
native build, container, full suite, source-fork edit, merge, or push was used
for this review.
