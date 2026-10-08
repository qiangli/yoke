# Band filter acceptance — story #1179

Verified on 2026-10-08 for Sprint #379, Story-ID `a18f4bef8397`.
Candidate: `4588b8c174cf780b3882b6c939dca6f401cafd77`.

The requested implementation is already delivered by ancestor commit
`7ca30fd9f85bb75adcd6ad6368dd5a7a96d3064e`. No additional production
change is needed. The original causes were missing model-list flag
registration/filtering and hard-coded help text retaining the L4 ceiling.
The existing fix registers both flags, validates nonzero values against
`MaxBand`, rejects conflicting filters, and filters before text/JSON output.
Agent list and model add/set help now include L5.

## Focused verification

Run locally on macOS/arm64, using the umbrella Go workspace:

```sh
go test ./pkg/fleet -run '^(TestModelsListBandFilters|TestModelsListBandRejectsConflictAndRange|TestBandHelpNamesMaxBand|TestAgentListBandFilterReadsTheEffectiveBand)$' -count=1 -v
```

Exit 0, all four tests passed. These cover exact/minimum model bands,
text/JSON agreement, conflicting/out-of-range model filters, L5 help, and
agent filtering against the effective ladder band.

For the red control, copied current `pkg/fleet/cli.go` and `cli_write.go`
to a temporary directory, then reversed the production-only diff of
`7ca30fd` there with `git apply --reverse`. Mapped those two temporary
files through a Go `-overlay` JSON file; the checkout remained unchanged.

```sh
go test -overlay "$overlay" ./pkg/fleet -run '^(TestModelsListBandFilters|TestModelsListBandRejectsConflictAndRange|TestBandHelpNamesMaxBand)$' -count=1 -v
```

Exit 1, with all three regression tests failing for the expected reasons:

```text
TestModelsListBandFilters: list --band 5: unknown flag: --band
TestModelsListBandRejectsConflictAndRange: conflict error = unknown flag: --band, want alternatives
TestBandHelpNamesMaxBand: agent list --band help = "only agents in exactly this band (1-4)", want 1-5
```

This is a reversal control on the current candidate, not a claim that the
entire historical parent was rebuilt. The passing run uses no overlay.

Installed-binary smoke checks also passed (exit 0):

```sh
bashy model list --band 5 --json
bashy model list --min-band 5 --json
bashy agent list --help
```

Both model commands returned an L5 row; agent help showed `(1-5)` for
both filters. Unit tests above establish behavior of the source candidate;
the smoke checks separately establish behavior of the installed binary.

## Module gates

Run locally on macOS/arm64 against the candidate above, with each process
exit captured directly (no output pipeline):

```sh
go build ./...
go vet ./...
```

Both exited 0 without diagnostics. No full test suite or native Windows/Linux
run was performed; this delivery records verification of existing code.

No push or CI dispatch was performed for this acceptance-only delivery;
there is no new CI run URL. The conductor owns integration and story closure.
