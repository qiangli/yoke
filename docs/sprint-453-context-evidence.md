# Sprint 453 context evidence — c537aeb13da6 (#2271)

Holder: s453-context-muse. Base 76fb5d2f incl 284 M4 3fa07647.
Preserved 751450c6 (pkg/recall/memory_context.go) kept and corrected per
manager early review (5 items ACKed before editing).

## Seam (exact)

- `recall.MemorySource{Target,Index,Session,Dir,Mode,Ring,Tags,Repos,OS,Since,Until,Wait,Timeout,Call}`
  + `recall.MemoryContext(ctx,q,src,readers...) ContextResult` —
  sole lexical pipeline is `kbmemory.Retrieve` (explicit Target+Index, never provisioned).
- Additive `omitempty` only: `Hit.Retrieval *BlockRetrieval{mode,score,target,estimate,revision,index_revision,stale}`,
  `ContextBlock.retrieval`, `ContextResult.freshness/truncation`.
  `context_version` stays 1. No new top-level error member: `ContextResult.MemoryError`
  is internal state (`json:"-"`, never marshalled) per §10.3.
- Five review corrections applied: (1) `estimate:true` in new BlockRetrieval labels
  block Tokens as estimate; prompt text marks new-path blocks `(ref, ~N tokens estimate)`;
  legacy lines/text/JSON unchanged. (2) `SourceRef.URI` = verbatim stable record ID;
  canonical target in `retrieval.target` + top-level freshness (never target+region URI).
  (3) `truncation.omitted` counts served memory hits with no surviving block (per-ring
  K cap, budget ladder) alongside Retrieve cuts. (4) Pre-budget dedup on
  (target, record ID) ONLY — post-budget (ring, ref) collapse removed, so a kb-ring
  hit sharing a memory ref string (distinct target) survives; verbatim refs preserved.
  (5) Explicit-collection Retrieve failure withholds legacy blocks, records the broken
  memory ring, keeps the code in internal `MemoryError{code,message}`, and `kb context`
  RunE returns typed `*kbmemory.ExitError` mapped by `MemoryExitCode`
  (not_found/busy→1, invalid_input/unsupported→2, claim_held→9, timeout→124,
  cancelled→130) — no `os.Exit` in library, no successful legacy fallback.
- `kb context` flags: `--memory-target/--memory-index` (index required, never
  provisioned), `--memory-session/--memory-dir/--memory-mode`,
  `--memory-tags/--memory-repos/--memory-os/--memory-since/--memory-until`,
  `--memory-wait/--memory-timeout`. Empty `--memory-target` keeps legacy assembly
  byte-identical. CLI/MCP boundary consumes the returned `ExitError` code (same
  table); Bashy consumer wiring stays phase 2.

## Commands run (local only, dev-host)

```
gofmt -l pkg/recall/                                    # clean, no output
CGO_ENABLED=0 GOMAXPROCS=4 GOFLAGS=-p=2 go test -short -count=1 ./pkg/recall -run 'TestContext|TestMemoryContext'
  # ok github.com/qiangli/yoke/pkg/recall 1.767s, DIRECT EXIT 0
go vet ./pkg/recall/                                   # exit 0, no output
grep -rn "dedupeBlocksByIdentity|memory_error|os.Exit" pkg/recall/*.go (non-test):
  # only pre-existing recall os.Exit(ExitBrokenRing) + one os.Exit comment remain;
  # golden testdata/ diff vs 76fb5d2f is empty (byte-unchanged)
```

Focused tests (pkg/recall/memory_context_test.go, stubbed Retrieve — no backend,
short-safe): provenance URI-verbatim/target/estimate/JSON keys, pre-budget dedup
(duplicates never reported as cuts), memory+legacy same-ref survival (no
post-assembly collapse) + cross-ring unrelated-identity survival, budget bound,
Retrieve-truncation + assembler-omitted truncation, both owner modes
(persistent-direct + explicit-owner pass-through), explicit failure withholds legacy
+ internal structured code + exit mapping (busy/claim_held/timeout/cancelled/unsupported/not_found),
failed-packet JSON carries NO memory_error member, unselected-ring refusal,
cancellation code 130, prompt-text estimate label (legacy text clean),
CLI flag registration + target-without-index refusal,
legacy envelope key-absence (`retrieval/freshness/truncation/memory_error`).
Existing TestContextEnvelopeGolden passes unchanged (golden byte-unchanged).

## Owed gates (manager runs remotely — NOT run on dev-host)

Whole `./pkg/recall` + `./pkg/kbmemory` suites, native-process tests,
live CLI/MCP/golden gates (incl. real `--memory-target` CLI exit-code observation
via returned `*kbmemory.ExitError` and MCP exit-code mapping).

## Scope kept

Own files only: pkg/recall/context.go, cmd.go, memory_context.go,
memory_context_test.go + this evidence doc. (recall.go additive Hit.Retrieval seam
from prior commit untouched.) No kbmemory capture/retrieval/maintenance edits,
no Bashy consumer/atlas/dispatch/module-pin edits, no second context command,
no embeddings, no KB.md generated file. Same holder retains story for corrections
and consumer phase.
