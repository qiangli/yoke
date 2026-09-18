# Fleet evidence invariant

The coder produces a change. An adversarial pair may produce additional executable
evidence. Only a command gate decides whether that combined tree may merge.

For weave, adversarial review is opt-in:

```text
bashy weave pull <run> --review-agent <agent>
bashy weave autopilot ... --review-agent <agent>
bashy weave heartbeat ... --review-agent <agent>
```

On a submitted run, pull resolves a reviewer that differs from the coding agent,
runs `bashy pair --diff <base-ref> --agent <reviewer>` inside the run workspace,
and commits changes left by the pair. It then re-collects terminal evidence and
runs the existing verify gate. The existing suite gate still runs in the normal
merge/reset path. A failing test therefore blocks the merge through a command,
not through a model verdict.

The queue item records `coding_agent`, `review_agent`, and
`review_added_test`. If the requested reviewer resolves to the coder, weave picks
a different registered agent, preferring a different tool and model. Passing
`--review-agent auto` explicitly asks weave to derive the reviewer. If no distinct
agent exists, review fails closed. With no `--review-agent`, weave performs no pair
review and retains its previous behavior.

The invariant is strict:

- the pair can write evidence but cannot approve;
- the gate can pass or fail but does not write the change;
- the coder cannot review itself;
- prose and process exit alone never substitute for executable evidence.
