# `kb ask` protocol

`kb ask QUESTION --json` returns `bashy-kb-ask-step-v1` with a random `session` ID, `round: 1`, accumulated visible `evidence` (`ref`, `ring`, `text`, and source `path`), `open_questions`, visible `contradictions` (`a`, `b`, `relation: "contradicts"`), `suggested_follow_ups`, and `coverage`. Coverage is the fraction of distinct original-question terms present in visible evidence. Retrieval uses the existing context ranker over repo and host note/page rings. A missing store contributes no evidence.

Continue with `kb ask --session ID --continue "QUERY" --json`. The command saves each round outside all kb rings under the bashy state directory's `ask/` directory. It stops suggesting follow-ups at `--min-coverage` (default `0.6`) or after `--rounds` (default `4`); a caller closes with `kb ask --session ID --final --json`. Sessions are bound to the current principal and cannot be reused after closing.

The final `bashy-kb-ask-final-v1` envelope contains `answer`, `citations` (`ref` and `path`), `coverage`, and `abstained`. The default answer quotes visible evidence with its ref; it does not synthesize claims. Every citation is rechecked through `kb show` for the same principal at finalization. Low coverage, contradictions between visible refs, expired or stale records, denied or missing refs cause `abstained: true` with `reasons` and `blocking_refs`, an empty answer, and exit code 0. Hidden content is never placed in evidence or follow-ups.

`--reasoner local` optionally calls the local Ollama completion seam to propose follow-up queries, runs the same retrieval and validation rounds, then returns the final envelope. Without that flag the command never calls a model.
