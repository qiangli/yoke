# Bounded pure-Go Ollama model loading

Sprint 436 · Story 2020 (`10e532fac3b7`).

Yoke registers the llama model's Ollama tokenizer before starting its embedded
server. In a build without cgo, the server now tries that tokenizer even when
`OLLAMA_NEW_ENGINE` is unset. A supported llama GGUF uses `--ollama-engine` and
starts the external runner. Models without a registered/supported tokenizer
fail before runner startup with `model has no supported pure-Go tokenizer`.
This is a bounded llama path; it does not claim compatibility with every GGUF.

The built-in `"json"` grammar remains available. JSON Schema conversion depends
on cgo and now returns an explicit unsupported error in pure-Go builds instead
of misreporting a valid schema as invalid. The official v0.21.2 runner source is
unchanged.

## Focused gate

Run from `external/ollama/src`:

```text
CGO_ENABLED=0 go test -count=1 ./llm
CGO_ENABLED=0 go vet ./llm
```

Run from Yoke root:

```text
CGO_ENABLED=0 go test -count=1 ./external/ollama/runner_embed
CGO_ENABLED=0 go vet ./external/ollama/runner_embed
```

The `llm` test writes a two-token, zero-tensor GGUF. It proves model selection,
re-exec with `runner --ollama-engine`, and a `/health` response from a helper
child. It tests clear unsupported-tokenizer and JSON-Schema failures. The
`runner_embed` test checks that a hash/identity/bundle-verified extracted
executable receives those arguments, and existing tests reject unsigned and
tampered data. Test signature verification uses an injected fixture verifier;
the real release remains disabled until hosted signing and native inference
gates pass. The zero-tensor GGUF is a protocol fixture, not an inference model.

The root `external/ollama` package cannot be compiled with `CGO_ENABLED=0` on
Dragon at this pin because upstream MLX and discovery packages reference
symbols unavailable in that mode. This gate therefore uses the focused `llm`
and runner extraction packages; it does not claim a whole-binary or native
inference result. The release gate must resolve that build and exercise a
licensed tiny inference model against the hosted signed runner.
