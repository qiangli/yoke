# Bashy service supply-chain policy

**Status:** Active (Sprint 434, Story 2005)

## Principle

Bashy never executes a third-party-built binary for its own services. Every
managed service that bashy starts must be built from source that is reviewed
in this repository. The supply-chain trust root is the repository's git
history, not a downloaded release artifact or a checksum sidecar served from
the same origin.

## Ollama (tier 3, sandbox)

*   **Allowed:** the embedded runner compiled from source:
    `external/ollama/runner_embed` (gzip-compressed runner, embedded via
    `-tags embed_runner` — see `external/ollama/runner_embed/doc.go`) +
    `external/ollama/src` (the `github.com/ollama/ollama` submodule, MIT) +
    `external/ollama/ollama_component.go` (the in-process `ollamaserver.Serve`
    wrapper).

*   **Forbidden:** downloading or execing an upstream `ollama/ollama`
    release archive (e.g. `ollama-linux-amd64.tar.zst`,
    `ollama-darwin.tgz`, `ollama-windows-amd64.zip`) via
    `external/registry` (`binmgr` GitHubSpec `ollama/ollama` v0.40.1) or
    any `registry.Ollama` / `OllamaVersion` / `ollama/ollama` caller
    outside `external/ollama/src`, and any `PATH` / `OLLAMA_BIN` fallback
    that execs a host-installed `ollama` binary.

*   **Fail-closed behavior:** if the embedded runner is not compiled into
    the binary (no `-tags embed_runner`, or platform where it is
    unavailable), `bashy ollama` and the managed service (`bashy ollama
    serve`, the model door's `ExecEngine` argv `[bashy ollama serve]`, and
    `external/ollama/ollama.go:Resolve` / `Run`) **fail closed** with an
    error that names this policy file
    (`docs/bashy-service-supply-chain-policy.md`). They never fall back to
    `PATH`, `OLLAMA_BIN`, well-known locations, or a binmgr download, and
    never attempt network access.

*   **Update checks:** upstream cloud / update checks remain off. The
    embedded server path sets `OLLAMA_NO_CLOUD=1` (disables remote
    inference and web search — see
    `https://github.com/ollama/ollama/blob/main/docs/faq.mdx#how-can-i-disable-ollama-cloud`)
    and does not start Ollama Desktop's updater, as recorded in
    `external/ollama/managed.go:applyManagedEnv` / `managedServeEnv`
    and `docs/service-runtime-privacy.md`. Do not regress this.

## Verification

*   `external/registry/ollama.go` resolves to a policy error, never a
    `binmgr.Tool`.
*   `pkg/binmgr/pins.go` carries no `ollama@v0.40.1` pins.
*   `external/ollama/ollama.go:Resolve` and `Run` fail closed with the
    policy name, never probing `PATH`.
*   `external/ollama/cmd.go` (`ollama serve` / `run` / `version`) fails
    closed when the embedded runner is absent, never calling `Resolve` or
    downloading.
*   Unit tests proving refusal without network:
    `external/registry/registry_test.go:TestOllamaRegistryRefusesDownload`,
    `external/ollama/ollama_test.go:TestResolve_RefusesPerSupplyChainPolicy`,
    `external/ollama/cmd_embedded_test.go:TestServeFailsClosedWithoutRunner`.
*   Gate: `go vet` + `go test -count=1 ./external/ollama/... ./external/registry/... ./pkg/binmgr/...` green.

## History

Audit row `docs/bashy-supply-chain-audit.md:22` (umbrella Sprint 434).
Migration Story 2005 (Story-ID 3054b43bab36) removed the downloaded
official-archive fallback and pinned the service to the embedded build.
