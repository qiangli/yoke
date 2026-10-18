# Sprint 467 / Story 2370: post-install steering evidence

Standalone evidence follow-up for `e6746b0025d9`, worker `codex-gpt6-astra`.
This file can be added independently of the unpublished consumer evidence
ancestors. Integrate this evidence commit alone; do not replay those ancestors.
Earlier published resume and consumer pin fixes remain unchanged.

## Post-install Codex exec transport correction (2026-10-18)


Candidate source commit: `145e47a17d11e4b2f6ccfb7edba869fd736db54e`.
Parent: `0ec8d689`. Actual worker identity: `codex-gpt6-astra`.
This follow-up preserves all published resume/pin commits and private backup refs.

Acceptance clarification: supervisor MB6269, recorded by the manager in the
canonical story at 01:56 PDT, accepts truthful queued reporting plus explicit
same-holder resume incorporation. Immediate live transport is separate story
`c3138d41`, outside Sprint 467. No live model consumption is claimed here.
Manager approved the narrow control-socket allocation hunk at 01:54 PDT.

The installed observation/coordination binary was
`<user-home>/.local/bin/bashy`, build `8bd225e`. Tests below run this workspace's
Go code and a deterministic Go test executable standing in for the one-shot
worker. They do not call a provider or metered API. The current run396 queue
record has no instance UUID, so an exact instance-address receipt cannot be
asserted for this worker. This was reported to the manager; the bare shared
binding is not treated as an unambiguous mailbox address.

Read-only inspection of the reported sqlite-fork run2 log confirmed launch at
08:45:50Z, pointer correction echoed at lines51/52, superseded bool commit
`7e5eaf8c20921626d5063c8d93df29c2cd5408ea` at lines72/78, and a completed turn at
line97. Echo is not consumer receipt. No other run was changed.

Cause: weave renders the tool's headless Exec template. Codex exec ignores
mid-run terminal input; allocating a PTY does not select its separate
interactive SteerExec template. The prior control-file-only test could not
establish consumption. The new process test keeps a one-shot child alive,
sends say/comment, reads the child's actual prompt receipt, then explicitly
resumes the same workspace and checks the correction commit and preserved file.

Minimal fix: actual Codex exec launches do not allocate or advertise a live
control socket. Say/comment on legacy Codex exec records also retain prose
without writing it to their old control socket, reporting `delivered:false`.
Existing resume prompt replay carries that input. Interactive Codex and other
tools, including supported streaming-input paths, retain existing behavior.
The argv check skips option values and does not confuse a prompt after `--`
with the exec subcommand. No TUI migration or automatic restart was introduced.
The submitted-run handoff trap remains with its separate owner.

Red, before production edits (exit1):
```sh
go test ./pkg/weave -run '^TestCodexExec(SteeringRequiresResume|LegacySocketDoesNotAcceptSteering)$' -count=1 -v
```
Relevant captured output (`.git/2370-codex-red.log`):
```text
headless Exec advertised a live steering socket: <temporary socket>; say={... "delivered": true ...}
headless say must report queued only: {... "delivered": true ...}
--- FAIL: TestCodexExecSteeringRequiresResume
comment claims live delivery: 0 {... "delivered": true ...}
headless control got prose: "STEERING-2370: queued for resume\r\nSTEERING-2370: queued for resume\r\n" <nil>
--- FAIL: TestCodexExecLegacySocketDoesNotAcceptSteering (0.25s)
FAIL github.com/qiangli/yoke/pkg/weave 2.016s
```
The first generic one-shot reproducer also failed before narrowing to Codex
(`.git/2370-live-red.log`, 3.086s). After the initial green, the argv guard was
refined and coverage added for interactive/config/prompt/other-tool cases and
the live presence card. The final gate below covers the committed source.

Green on the development host, filtered tests only (exit0):
```sh
go test ./pkg/weave ./pkg/bus -run '^(TestCodexExecSteeringRequiresResume|TestCodexExecLegacySocketDoesNotAcceptSteering|TestCodexExecControlCapability|TestResumeSteeringReceipt|TestSaySteeringSurvivesStoppedRun|TestLiveSteeringCommentAndSayReceipt|TestWeaveInstanceSteeringUsesOwningSocket)$' -count=1 -v
```
Relevant output (`.git/2370-live-final-green.log`):
```text
--- PASS: TestResumeSteeringReceipt (2.73s)
    --- PASS: TestResumeSteeringReceipt/comment (0.79s)
    --- PASS: TestResumeSteeringReceipt/say (1.05s)
    --- PASS: TestResumeSteeringReceipt/mb (0.89s)
--- PASS: TestSaySteeringSurvivesStoppedRun (0.26s)
--- PASS: TestLiveSteeringCommentAndSayReceipt (0.49s)
headless input absent; resumed child incorporated say/comment, HEAD 31e4f2ff0523c97652eba4ec0e02afedc2954202 -> 2326a77fa94b63b80c8c9b2f77043114d9cb5a62
--- PASS: TestCodexExecSteeringRequiresResume (1.17s)
--- PASS: TestCodexExecLegacySocketDoesNotAcceptSteering (0.27s)
--- PASS: TestCodexExecControlCapability (0.00s)
PASS
ok github.com/qiangli/yoke/pkg/weave 5.928s
--- PASS: TestWeaveInstanceSteeringUsesOwningSocket (0.03s)
PASS
ok github.com/qiangli/yoke/pkg/bus 0.381s
```
The MB resume fixture resolved its clone to its own temporary UUID and verified
actual argv receipt and a new commit without consuming the mailbox cursor.
The old live control-file test remains transport-only evidence.

`git diff --check` passed. Whole-package and remote OS gates belong to the
manager and were not rerun locally. The preexisting generated KB modification
was preserved privately as `.git/s467-2370-postinstall-KB.patch` before restoring
the tracked file; no private KB content is included in delivery. No pushes,
other-session restarts, other-story edits, or other-run mutations occurred.

## Standalone evidence correction, 02:11 PDT

Manager requested this prose-only follow-up after the previous evidence commit
conflicted with the published tree. The original tip `c518d4a4` is preserved
locally at `refs/local-backups/s467-e6746b0025d9-pre-standalone-evidence`;
older private backup refs are unchanged. The new startup KB diff is preserved
privately in `.git/s467-2370-evidence-followup-KB.patch` and excluded from delivery.
Paths in this standalone record use generic placeholders.

The manager reported initial integration as `f741e917`; the current integration
checkout carries the same source patch as `985764e5`. Stable patch IDs for
`git show --format= --binary <commit> -- pkg` on original `145e47a1` and
integrated `985764e5` both equal `ae6997b43f24545f01767922383d66369815c95c`.
The steering implementation and regression-test files are byte-identical at
those two commits. This follow-up changes no Go bytes relative to `145e47a1`,
so the earlier exact-source Windows gate remains applicable; no new Windows
result or code-test rerun is claimed. Manager explicitly waived code-test
reruns for this prose-only correction.

Validation for this follow-up: whitespace check, private-path/content scan,
Go source equality against `145e47a1`, and an isolated-index application check
against the manager integration tree. All must pass before submission.
The scope remains queued reporting and explicit same-holder resume; live
headless model consumption is outside this delivery.
