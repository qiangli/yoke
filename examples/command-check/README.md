# command-check

A minimal checker for the bashy command contract
([`docs/command-contract.md`](../../docs/command-contract.md)). It runs one
safe-to-repeat invocation with the contract's common options appended and
reports each requirement by ID: `conform`, `unsupported` (exit 2, which
conforms), `skip` or `FAIL`. Exit 0 when nothing fails, 1 otherwise.

```sh
bash register.sh                      # registers `bashy command-check` and its skill
bashy command-check -- bashy todo list
bashy command-check --json -- bashy --bashsharp bin/hello.bsh
```

It is a Bash# skill-script, so it can be shaped like any other command. It
uses only reads and process execution (no files), so it fits an app whose
shaping cap is `read`.

## Conformance through shaping (opt-in)

Conformance is optional. An app owner who wants it adds a check to the app's
`checks/`; `bashy shape verify` then runs it on every candidate.
`hello-world-candidate/` is that change for the Hello World shaping example:

- `bin/hello.bsh`: the app's one action (print the greeting) as a conforming
  command;
- `tools/command-check.bsh`: a copy of this checker, because checks run
  contained (PATH `/usr/bin:/bin`, HOME in the clone, no registered commands);
- `checks/command-contract.bsh`: runs the copy on `bin/hello.bsh` through
  `$BASH`, the interpreter running the check.

Imported with `bashy shape wish hello-world REQUEST --from WORKSPACE`, this
candidate verified with every check passing under the app's unchanged `read`
cap; a variant whose `hello` ignored unknown options was refused on C5/C6.

## Limits

- From outside, a command that silently ignores `--dry-run` or `--explain`
  looks like one that supports them, so C7/C9 can read `conform` falsely.
  The definite signal is C5/C6 (invalid combinations must exit 2).
- C14 reads effects only for `bashy NAME` targets; other commands declare
  effects in their own record.
- The invocation must be safe to run several times: the checker runs it about
  eight times.
