# Signed runner extraction and Bashy dispatch handoff

Sprint 436 · Story 2019 (`a92979c1094f`) · source qualification `5e8c4687`.

`runner_embed.EnsureRunner` now requires a committed exact Bashy release tag,
workflow certificate identity, final executable SHA256, and embedded Sigstore
bundle. Both a cache hit and newly decompressed bytes must pass the same
pin, keyless certificate, signature, offline Fulcio/Rekor verification, and
final-byte check. The old writable `.sha256` sidecar is ignored. Extraction
never ad-hoc signs on macOS. These release fields are intentionally empty until
the separate hosted runner build, source audit, no-egress, signing, and review
gates succeed. Managed Ollama remains unavailable while they are empty.

## Separate Bashy executable story: early runner dispatch contract

The embedded Ollama server calls `os.Executable()` and starts a child with
`runner [--ollama-engine] --model PATH --port PORT`. Bashy must recognize the
leading `runner` argument **before** its normal command parsing, shell setup,
or agent initialization. It must call `EnsureRunner`, then execute only the
verified returned path with the complete original runner argument vector.
Pass through stdin, stdout, stderr, environment, working directory, exit
status, and termination signals. A missing release pin, signature, unsupported
platform, or failed cache verification must terminate nonzero without falling
back to `OLLAMA_BIN`, `PATH`, another cached binary, or the old in-process
runner. Test both the normal dispatch and a tampered offline cache. This is a
Bashy implementation story; Yoke does not modify the Bashy entrypoint here.

The final release integrator must replace the empty release fields with the
exact reviewed tag, workflow identity, and per-platform SHA and embed the
matching bundle. A compressed payload hash does not substitute for a final
executable pin.
