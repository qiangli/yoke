# Sprint 467 / Story 2370 / e6746b0025d9

Worker: `codex-gpt6-astra`; manager: `s467-manager`; workspace: `issue-396`.
Baseline: `4076e8d4`. Candidate SHA (production and tests): `6d00d762913eec3aab31592ca985be26da5b6aa8`.
This follow-up changes only evidence metadata; all green commands below apply
to the candidate code and tests unchanged.

## Findings and scope

- A resumed registered worker was launched with only the original body/title.
  Review comments and directed MB input never reached its prompt.
- Plain `weave say` required a live wrapper and did not preserve its text.
- MB correctly resolved the fixture clone family to its recorded instance UUID
  (display label `Lucia`), but the ownership card had no control socket and the
  weave transport card exposed only a tool/model binding. A different display
  label alone does not prove the wrong UUID was selected.
- Fix: include durable run comments and directed run mail on resume; preserve
  plain say input before best-effort transport; deliver comments live; publish
  the recorded instance principal on the weave transport card and match it in
  MB while continuing past its socket-less ownership card. No mailbox cursor
  advances merely because argv was assembled. Keystrokes remain live-only.
- Blind/arena prompts and conductor-supplied raw argv retain their contracts.
  A transport write proves transport delivery, not model comprehension.

## Recovery scope reported to manager

`weaveValidateHandoff` accepts killed/failed/allocated/no-op but rejects submitted
with kill/reconcile guidance. Kill requires working, matching the reported trap.
No lifecycle behavior changed here; the sprint subsequently allocated separate
story #2373 (`7423f50ae456`). Runs 348, 361, 392 and all other runs were untouched.
No real agent API was called; the resumed worker is a deterministic child test
executable using the actual registered-tool launch path.

## Red before production changes

Command (exit 1):
```sh
go test ./pkg/weave -run '^TestResumeSteeringReceipt$' -count=1 -v
```
```text
=== RUN   TestResumeSteeringReceipt
2026/10/18 00:22:58 WARN llmbudget: missing subscription plan limit metadata; allowing model=fable5 plan=anthropic:default
    weave_steering_test.go:62: resumed child received original prompt without steering:
        Original task: preserve unique work.
        
        ---
        WEAVE WORKER CONTRACT — non-negotiable; your work is INVISIBLE until you do these:
        1. COMMIT your changes to the CURRENT branch with git before you finish. Uncommitted
           or untracked files do NOT merge and the run is reported failed — a passing local
           build is not enough, the diff must be committed. Verify with 'git status' (clean)
           and 'git log --oneline -1' (your commit) before you stop.
        2. Do NOT push, do NOT switch branches, do NOT merge to main — the conductor pulls.
        3. If you cannot finish, COMMIT what works and state briefly what remains.
        4. Run the gate yourself before declaring done; a tool exiting 0 is not proof.
--- FAIL: TestResumeSteeringReceipt (4.31s)
FAIL
FAIL	github.com/qiangli/yoke/pkg/weave	5.266s
FAIL
```

Command (exit 1):
```sh
go test ./pkg/weave -run '^TestSaySteeringSurvivesStoppedRun$' -count=1 -v
```
```text
=== RUN   TestSaySteeringSurvivesStoppedRun
    weave_steering_test.go:79: late steering must queue durably: 4 weave say: run #1 has no live subagent (state="submitted")
--- FAIL: TestSaySteeringSurvivesStoppedRun (1.45s)
FAIL
FAIL	github.com/qiangli/yoke/pkg/weave	2.375s
FAIL
```

Command (exit 1):
```sh
go test ./pkg/weave -run '^TestResumeSteeringReceipt$/mb$' -count=1 -v
```
```text
=== RUN   TestResumeSteeringReceipt
=== RUN   TestResumeSteeringReceipt/mb
    weave_steering_test.go:83: clone resolved to recorded UUID 37e9ec4f-2c0f-4563-ba4e-8843816f3bec (label Lucia), receipt {Seq:1 ID: Kind:direct Label:Lucia Deliveries:[{To:Lucia Steered:false Reason:not running State:unverified Warning:no read evidence for Lucia — the board accepted this; that is not proof it was delivered}]}
2026/10/18 00:24:22 WARN llmbudget: missing subscription plan limit metadata; allowing model=fable5 plan=anthropic:default
    weave_steering_test.go:92: resumed child received original prompt without steering:
        Original task: preserve unique work.
        
        ---
        WEAVE WORKER CONTRACT — non-negotiable; your work is INVISIBLE until you do these:
        1. COMMIT your changes to the CURRENT branch with git before you finish. Uncommitted
           or untracked files do NOT merge and the run is reported failed — a passing local
           build is not enough, the diff must be committed. Verify with 'git status' (clean)
           and 'git log --oneline -1' (your commit) before you stop.
        2. Do NOT push, do NOT switch branches, do NOT merge to main — the conductor pulls.
        3. If you cannot finish, COMMIT what works and state briefly what remains.
        4. Run the gate yourself before declaring done; a tool exiting 0 is not proof.
--- FAIL: TestResumeSteeringReceipt (3.25s)
    --- FAIL: TestResumeSteeringReceipt/mb (3.25s)
FAIL
FAIL	github.com/qiangli/yoke/pkg/weave	4.157s
FAIL
```

Command (exit 1):
```sh
go test ./pkg/bus -run '^TestWeaveInstanceSteeringUsesOwningSocket$' -count=1 -v
```
```text
=== RUN   TestWeaveInstanceSteeringUsesOwningSocket
    weave_steering_test.go:34: no actual worker receipt: socket="" body="" result={Seq:1 ID: Kind:direct Label:Lucia Deliveries:[{To:Lucia Steered:false Reason:session has no control socket (not bashy-launched) State:unverified Warning:no read evidence for Lucia — the board accepted this; that is not proof it was delivered}]}
--- FAIL: TestWeaveInstanceSteeringUsesOwningSocket (0.08s)
FAIL
FAIL	github.com/qiangli/yoke/pkg/bus	0.494s
FAIL
```

## Preserved startup workspace change

The unrelated harness-generated `KB.md` refresh is excluded from delivery.
Its exact patch is preserved locally at
`/tmp/s467-e6746b0025d9-startup-KB.patch`, outside the repository.
The generated file was restored only after verifying that backup matched its diff.

## Green verification (development host, filtered tests only)

All commands below exited 0. The child captures actual launch argv, commits a
correction only when the marker is present, and the parent asserts a changed
HEAD, correction contents, preserved prior unique file, no foreign mail and no
false cursor acknowledgement. Live comment/say and MB tests inspect bytes
written by the real control transport to its supported file receiver.
Manager owns whole-package remote gates and crossvet; these were not run here.

Command (exit 0):
```sh
go test ./pkg/weave ./pkg/bus -run '^(TestResumeSteeringReceipt|TestSaySteeringSurvivesStoppedRun|TestLiveSteeringCommentAndSayReceipt|TestWeaveInstanceSteeringUsesOwningSocket|TestResumeHandoffFlagRequiresResume|TestResumeHandoffNativePreservesWorkspaceAndImmutableBinding|TestResumeHandoffRefusalsLeaveIdentityAndIndexUntouched|TestExpandAgentSelectsTheModel|TestPostDirected_InstanceReaderOnlyByExplicitUUID|TestDeliveryState_AllSixAreReachable|TestResolveRecipient_NameResolvesToUUIDBeforeAppend)$' -count=1 -v
```
```text
=== RUN   TestExpandAgentSelectsTheModel
--- PASS: TestExpandAgentSelectsTheModel (0.08s)
=== RUN   TestResumeHandoffFlagRequiresResume
--- PASS: TestResumeHandoffFlagRequiresResume (0.13s)
=== RUN   TestResumeHandoffNativePreservesWorkspaceAndImmutableBinding
--- PASS: TestResumeHandoffNativePreservesWorkspaceAndImmutableBinding (0.99s)
=== RUN   TestResumeHandoffRefusalsLeaveIdentityAndIndexUntouched
=== RUN   TestResumeHandoffRefusalsLeaveIdentityAndIndexUntouched/working
=== RUN   TestResumeHandoffRefusalsLeaveIdentityAndIndexUntouched/live-wrapper
=== RUN   TestResumeHandoffRefusalsLeaveIdentityAndIndexUntouched/live-child
=== RUN   TestResumeHandoffRefusalsLeaveIdentityAndIndexUntouched/live-session
=== RUN   TestResumeHandoffRefusalsLeaveIdentityAndIndexUntouched/reservation
=== RUN   TestResumeHandoffRefusalsLeaveIdentityAndIndexUntouched/branch
=== RUN   TestResumeHandoffRefusalsLeaveIdentityAndIndexUntouched/conflict
=== RUN   TestResumeHandoffRefusalsLeaveIdentityAndIndexUntouched/unknown-agent
--- PASS: TestResumeHandoffRefusalsLeaveIdentityAndIndexUntouched (6.65s)
    --- PASS: TestResumeHandoffRefusalsLeaveIdentityAndIndexUntouched/working (0.60s)
    --- PASS: TestResumeHandoffRefusalsLeaveIdentityAndIndexUntouched/live-wrapper (0.59s)
    --- PASS: TestResumeHandoffRefusalsLeaveIdentityAndIndexUntouched/live-child (0.65s)
    --- PASS: TestResumeHandoffRefusalsLeaveIdentityAndIndexUntouched/live-session (0.97s)
    --- PASS: TestResumeHandoffRefusalsLeaveIdentityAndIndexUntouched/reservation (0.75s)
    --- PASS: TestResumeHandoffRefusalsLeaveIdentityAndIndexUntouched/branch (0.80s)
    --- PASS: TestResumeHandoffRefusalsLeaveIdentityAndIndexUntouched/conflict (0.93s)
    --- PASS: TestResumeHandoffRefusalsLeaveIdentityAndIndexUntouched/unknown-agent (1.36s)
=== RUN   TestResumeSteeringReceipt
=== RUN   TestResumeSteeringReceipt/comment
2026/10/18 00:27:26 WARN llmbudget: missing subscription plan limit metadata; allowing model=fable5 plan=anthropic:default
    weave_steering_test.go:111: actual resumed child receipt: Original task: preserve unique work.
        
        ---
        WEAVE STEERING AND REVIEW
        Incorporate the following run input before resubmitting. Resolve any conflicting instructions explicitly.
        
        [2026-10-18T07:27:25Z] codex-gpt6-astra-f (review): STEERING-2370: incorporate the requested correction before resubmitting
        
        ---
        WEAVE WORKER CONTRACT — non-negotiable; your work is INVISIBLE until you do these:
        1. COMMIT your changes to the CURRENT branch with git before you finish. Uncommitted
           or untracked files do NOT merge and the run is reported failed — a passing local
           build is not enough, the diff must be committed. Verify with 'git status' (clean)
           and 'git log --oneline -1' (your commit) before you stop.
        2. Do NOT push, do NOT switch branches, do NOT merge to main — the conductor pulls.
        3. If you cannot finish, COMMIT what works and state briefly what remains.
        4. Run the gate yourself before declaring done; a tool exiting 0 is not proof.
=== RUN   TestResumeSteeringReceipt/say
2026/10/18 00:27:31 WARN llmbudget: missing subscription plan limit metadata; allowing model=fable5 plan=anthropic:default
    weave_steering_test.go:111: actual resumed child receipt: Original task: preserve unique work.
        
        ---
        WEAVE STEERING AND REVIEW
        Incorporate the following run input before resubmitting. Resolve any conflicting instructions explicitly.
        
        [2026-10-18T07:27:30Z] codex-gpt6-astra-f (steering): STEERING-2370: incorporate the requested correction before resubmitting
        
        ---
        WEAVE WORKER CONTRACT — non-negotiable; your work is INVISIBLE until you do these:
        1. COMMIT your changes to the CURRENT branch with git before you finish. Uncommitted
           or untracked files do NOT merge and the run is reported failed — a passing local
           build is not enough, the diff must be committed. Verify with 'git status' (clean)
           and 'git log --oneline -1' (your commit) before you stop.
        2. Do NOT push, do NOT switch branches, do NOT merge to main — the conductor pulls.
        3. If you cannot finish, COMMIT what works and state briefly what remains.
        4. Run the gate yourself before declaring done; a tool exiting 0 is not proof.
=== RUN   TestResumeSteeringReceipt/mb
    weave_steering_test.go:88: clone resolved to recorded UUID e651dd27-aa7b-4356-94bb-eee914e21519 (label Lucia), receipt {Seq:1 ID: Kind:direct Label:Lucia Deliveries:[{To:Lucia Steered:false Reason:not running State:unverified Warning:no read evidence for Lucia — the board accepted this; that is not proof it was delivered}]}
2026/10/18 00:27:37 WARN llmbudget: missing subscription plan limit metadata; allowing model=fable5 plan=anthropic:default
    weave_steering_test.go:111: actual resumed child receipt: Original task: preserve unique work.
        
        ---
        WEAVE STEERING AND REVIEW
        Incorporate the following run input before resubmitting. Resolve any conflicting instructions explicitly.
        
        [mb:1] manager: STEERING-2370: incorporate the requested correction before resubmitting
        
        ---
        WEAVE WORKER CONTRACT — non-negotiable; your work is INVISIBLE until you do these:
        1. COMMIT your changes to the CURRENT branch with git before you finish. Uncommitted
           or untracked files do NOT merge and the run is reported failed — a passing local
           build is not enough, the diff must be committed. Verify with 'git status' (clean)
           and 'git log --oneline -1' (your commit) before you stop.
        2. Do NOT push, do NOT switch branches, do NOT merge to main — the conductor pulls.
        3. If you cannot finish, COMMIT what works and state briefly what remains.
        4. Run the gate yourself before declaring done; a tool exiting 0 is not proof.
--- PASS: TestResumeSteeringReceipt (14.81s)
    --- PASS: TestResumeSteeringReceipt/comment (4.31s)
    --- PASS: TestResumeSteeringReceipt/say (6.06s)
    --- PASS: TestResumeSteeringReceipt/mb (4.44s)
=== RUN   TestSaySteeringSurvivesStoppedRun
--- PASS: TestSaySteeringSurvivesStoppedRun (0.95s)
=== RUN   TestLiveSteeringCommentAndSayReceipt
=== RUN   TestLiveSteeringCommentAndSayReceipt/comment
=== RUN   TestLiveSteeringCommentAndSayReceipt/say
--- PASS: TestLiveSteeringCommentAndSayReceipt (2.52s)
    --- PASS: TestLiveSteeringCommentAndSayReceipt/comment (0.88s)
    --- PASS: TestLiveSteeringCommentAndSayReceipt/say (1.63s)
PASS
ok  	github.com/qiangli/yoke/pkg/weave	26.975s
=== RUN   TestDeliveryState_AllSixAreReachable
--- PASS: TestDeliveryState_AllSixAreReachable (0.00s)
=== RUN   TestResolveRecipient_NameResolvesToUUIDBeforeAppend
--- PASS: TestResolveRecipient_NameResolvesToUUIDBeforeAppend (0.01s)
=== RUN   TestPostDirected_InstanceReaderOnlyByExplicitUUID
--- PASS: TestPostDirected_InstanceReaderOnlyByExplicitUUID (0.00s)
=== RUN   TestWeaveInstanceSteeringUsesOwningSocket
--- PASS: TestWeaveInstanceSteeringUsesOwningSocket (0.03s)
PASS
ok  	github.com/qiangli/yoke/pkg/bus	1.335s
```

Command (exit 0):
```sh
go test ./pkg/weave -run '^TestResumeSteeringReceipt$' -count=1 -v
```
```text
=== RUN   TestResumeSteeringReceipt
=== RUN   TestResumeSteeringReceipt/comment
2026/10/18 00:28:47 WARN llmbudget: missing subscription plan limit metadata; allowing model=fable5 plan=anthropic:default
    weave_steering_test.go:155: resumed worker incorporated steering: HEAD f75ce4ab342c6e3cc658c7af5a42e0ddeb35f7d1 -> 0844457c559b4a7bc8b07cb4db454bf8385ea353
    weave_steering_test.go:156: actual resumed child receipt: Original task: preserve unique work.
        
        ---
        WEAVE STEERING AND REVIEW
        Incorporate the following run input before resubmitting. Resolve any conflicting instructions explicitly.
        
        [2026-10-18T07:28:47Z] codex-gpt6-astra-f (review): STEERING-2370: incorporate the requested correction before resubmitting
        
        ---
        WEAVE WORKER CONTRACT — non-negotiable; your work is INVISIBLE until you do these:
        1. COMMIT your changes to the CURRENT branch with git before you finish. Uncommitted
           or untracked files do NOT merge and the run is reported failed — a passing local
           build is not enough, the diff must be committed. Verify with 'git status' (clean)
           and 'git log --oneline -1' (your commit) before you stop.
        2. Do NOT push, do NOT switch branches, do NOT merge to main — the conductor pulls.
        3. If you cannot finish, COMMIT what works and state briefly what remains.
        4. Run the gate yourself before declaring done; a tool exiting 0 is not proof.
=== RUN   TestResumeSteeringReceipt/say
2026/10/18 00:28:50 WARN llmbudget: missing subscription plan limit metadata; allowing model=fable5 plan=anthropic:default
    weave_steering_test.go:155: resumed worker incorporated steering: HEAD 68e5c8a673602029faa7ead6332e141a811bc12e -> ee4b7bc1fc527f318b7156735c380fe3e2d0ea0e
    weave_steering_test.go:156: actual resumed child receipt: Original task: preserve unique work.
        
        ---
        WEAVE STEERING AND REVIEW
        Incorporate the following run input before resubmitting. Resolve any conflicting instructions explicitly.
        
        [2026-10-18T07:28:49Z] codex-gpt6-astra-f (steering): STEERING-2370: incorporate the requested correction before resubmitting
        
        ---
        WEAVE WORKER CONTRACT — non-negotiable; your work is INVISIBLE until you do these:
        1. COMMIT your changes to the CURRENT branch with git before you finish. Uncommitted
           or untracked files do NOT merge and the run is reported failed — a passing local
           build is not enough, the diff must be committed. Verify with 'git status' (clean)
           and 'git log --oneline -1' (your commit) before you stop.
        2. Do NOT push, do NOT switch branches, do NOT merge to main — the conductor pulls.
        3. If you cannot finish, COMMIT what works and state briefly what remains.
        4. Run the gate yourself before declaring done; a tool exiting 0 is not proof.
=== RUN   TestResumeSteeringReceipt/mb
    weave_steering_test.go:117: clone resolved to recorded UUID 4c62ba77-39d0-49f9-aaf2-1f0cd6af684a (label Lucia), receipt {Seq:1 ID: Kind:direct Label:Lucia Deliveries:[{To:Lucia Steered:false Reason:not running State:unverified Warning:no read evidence for Lucia — the board accepted this; that is not proof it was delivered}]}
2026/10/18 00:28:53 WARN llmbudget: missing subscription plan limit metadata; allowing model=fable5 plan=anthropic:default
    weave_steering_test.go:155: resumed worker incorporated steering: HEAD 34ef009a88eaa3c2ba9b38917d5befe0593d889c -> 9c50015966b5353409eabcc535e401e2cf7a1e37
    weave_steering_test.go:156: actual resumed child receipt: Original task: preserve unique work.
        
        ---
        WEAVE STEERING AND REVIEW
        Incorporate the following run input before resubmitting. Resolve any conflicting instructions explicitly.
        
        [mb:1] manager: STEERING-2370: incorporate the requested correction before resubmitting
        
        ---
        WEAVE WORKER CONTRACT — non-negotiable; your work is INVISIBLE until you do these:
        1. COMMIT your changes to the CURRENT branch with git before you finish. Uncommitted
           or untracked files do NOT merge and the run is reported failed — a passing local
           build is not enough, the diff must be committed. Verify with 'git status' (clean)
           and 'git log --oneline -1' (your commit) before you stop.
        2. Do NOT push, do NOT switch branches, do NOT merge to main — the conductor pulls.
        3. If you cannot finish, COMMIT what works and state briefly what remains.
        4. Run the gate yourself before declaring done; a tool exiting 0 is not proof.
--- PASS: TestResumeSteeringReceipt (12.37s)
    --- PASS: TestResumeSteeringReceipt/comment (5.17s)
    --- PASS: TestResumeSteeringReceipt/say (3.71s)
    --- PASS: TestResumeSteeringReceipt/mb (3.46s)
PASS
ok  	github.com/qiangli/yoke/pkg/weave	18.435s
```

Command (exit 0):
```sh
go test ./pkg/bus -run '^(TestWeaveInstanceSteeringUsesOwningSocket|TestDeliveryState_AllSixAreReachable|TestResolveRecipient_NameResolvesToUUIDBeforeAppend|TestPostDirected_InstanceReaderOnlyByExplicitUUID)$' -count=1 -v
```
```text
=== RUN   TestDeliveryState_AllSixAreReachable
--- PASS: TestDeliveryState_AllSixAreReachable (0.00s)
=== RUN   TestResolveRecipient_NameResolvesToUUIDBeforeAppend
--- PASS: TestResolveRecipient_NameResolvesToUUIDBeforeAppend (0.01s)
=== RUN   TestPostDirected_InstanceReaderOnlyByExplicitUUID
--- PASS: TestPostDirected_InstanceReaderOnlyByExplicitUUID (0.00s)
=== RUN   TestWeaveInstanceSteeringUsesOwningSocket
--- PASS: TestWeaveInstanceSteeringUsesOwningSocket (0.06s)
PASS
ok  	github.com/qiangli/yoke/pkg/bus	0.570s
```

`git diff --check`: exit 0, no output.

## Review correction verification

Production and tests remain identical to candidate `bb543d46`.
The privacy correction removes the generated KB snapshot and uses a generic
development-host label. Focused rerun (exit 0):

```sh
go test ./pkg/weave ./pkg/bus -run '^(TestResumeSteeringReceipt|TestSaySteeringSurvivesStoppedRun|TestLiveSteeringCommentAndSayReceipt|TestWeaveInstanceSteeringUsesOwningSocket)$' -count=1 -v
```

```text
=== RUN   TestResumeSteeringReceipt
=== RUN   TestResumeSteeringReceipt/comment
2026/10/18 00:36:37 WARN llmbudget: missing subscription plan limit metadata; allowing model=fable5 plan=anthropic:default
    weave_steering_test.go:155: resumed worker incorporated steering: HEAD 93f0e251de010aa651ccaa4cbdc0ba302e6db574 -> c666eb16738344140371263d04cfba08bd5fe690
    weave_steering_test.go:156: actual resumed child receipt: Original task: preserve unique work.

        ---
        WEAVE STEERING AND REVIEW
        Incorporate the following run input before resubmitting. Resolve any conflicting instructions explicitly.

        [2026-10-18T07:36:37Z] codex-gpt6-astra-f (review): STEERING-2370: incorporate the requested correction before resubmitting

        ---
        WEAVE WORKER CONTRACT — non-negotiable; your work is INVISIBLE until you do these:
        1. COMMIT your changes to the CURRENT branch with git before you finish. Uncommitted
           or untracked files do NOT merge and the run is reported failed — a passing local
           build is not enough, the diff must be committed. Verify with 'git status' (clean)
           and 'git log --oneline -1' (your commit) before you stop.
        2. Do NOT push, do NOT switch branches, do NOT merge to main — the conductor pulls.
        3. If you cannot finish, COMMIT what works and state briefly what remains.
        4. Run the gate yourself before declaring done; a tool exiting 0 is not proof.
=== RUN   TestResumeSteeringReceipt/say
2026/10/18 00:36:39 WARN llmbudget: missing subscription plan limit metadata; allowing model=fable5 plan=anthropic:default
    weave_steering_test.go:155: resumed worker incorporated steering: HEAD 50d141618e656535fcbddc22c21b31b6bf8b931d -> a2d3c6510255ef2a5819af2d63607fab98c5f6fb
    weave_steering_test.go:156: actual resumed child receipt: Original task: preserve unique work.

        ---
        WEAVE STEERING AND REVIEW
        Incorporate the following run input before resubmitting. Resolve any conflicting instructions explicitly.

        [2026-10-18T07:36:39Z] codex-gpt6-astra-f (steering): STEERING-2370: incorporate the requested correction before resubmitting

        ---
        WEAVE WORKER CONTRACT — non-negotiable; your work is INVISIBLE until you do these:
        1. COMMIT your changes to the CURRENT branch with git before you finish. Uncommitted
           or untracked files do NOT merge and the run is reported failed — a passing local
           build is not enough, the diff must be committed. Verify with 'git status' (clean)
           and 'git log --oneline -1' (your commit) before you stop.
        2. Do NOT push, do NOT switch branches, do NOT merge to main — the conductor pulls.
        3. If you cannot finish, COMMIT what works and state briefly what remains.
        4. Run the gate yourself before declaring done; a tool exiting 0 is not proof.
=== RUN   TestResumeSteeringReceipt/mb
    weave_steering_test.go:117: clone resolved to recorded UUID 2c9c9c01-4f52-4d04-b2d5-7230efe29db4 (label Lucia), receipt {Seq:1 ID: Kind:direct Label:Lucia Deliveries:[{To:Lucia Steered:false Reason:not running State:unverified Warning:no read evidence for Lucia — the board accepted this; that is not proof it was delivered}]}
2026/10/18 00:36:41 WARN llmbudget: missing subscription plan limit metadata; allowing model=fable5 plan=anthropic:default
    weave_steering_test.go:155: resumed worker incorporated steering: HEAD bb0671a0f09c6a70c779683dd94d090e505c29b1 -> 1a401d2fc48d3d72304c4be403c177350b49574a
    weave_steering_test.go:156: actual resumed child receipt: Original task: preserve unique work.

        ---
        WEAVE STEERING AND REVIEW
        Incorporate the following run input before resubmitting. Resolve any conflicting instructions explicitly.

        [mb:1] manager: STEERING-2370: incorporate the requested correction before resubmitting

        ---
        WEAVE WORKER CONTRACT — non-negotiable; your work is INVISIBLE until you do these:
        1. COMMIT your changes to the CURRENT branch with git before you finish. Uncommitted
           or untracked files do NOT merge and the run is reported failed — a passing local
           build is not enough, the diff must be committed. Verify with 'git status' (clean)
           and 'git log --oneline -1' (your commit) before you stop.
        2. Do NOT push, do NOT switch branches, do NOT merge to main — the conductor pulls.
        3. If you cannot finish, COMMIT what works and state briefly what remains.
        4. Run the gate yourself before declaring done; a tool exiting 0 is not proof.
--- PASS: TestResumeSteeringReceipt (5.39s)
    --- PASS: TestResumeSteeringReceipt/comment (1.85s)
    --- PASS: TestResumeSteeringReceipt/say (1.73s)
    --- PASS: TestResumeSteeringReceipt/mb (1.81s)
=== RUN   TestSaySteeringSurvivesStoppedRun
--- PASS: TestSaySteeringSurvivesStoppedRun (0.45s)
=== RUN   TestLiveSteeringCommentAndSayReceipt
=== RUN   TestLiveSteeringCommentAndSayReceipt/comment
=== RUN   TestLiveSteeringCommentAndSayReceipt/say
--- PASS: TestLiveSteeringCommentAndSayReceipt (1.00s)
    --- PASS: TestLiveSteeringCommentAndSayReceipt/comment (0.47s)
    --- PASS: TestLiveSteeringCommentAndSayReceipt/say (0.53s)
PASS
ok  	github.com/qiangli/yoke/pkg/weave	7.651s
=== RUN   TestWeaveInstanceSteeringUsesOwningSocket
--- PASS: TestWeaveInstanceSteeringUsesOwningSocket (0.06s)
PASS
ok  	github.com/qiangli/yoke/pkg/bus	1.427s
```

## Unpublished history sanitation

Manager review required removing the private snapshot from every unpublished
ancestor, not just the final tree. Rebuilt this branch on `4076e8d4` with
source candidate `6d00d762913eec3aab31592ca985be26da5b6aa8` and explicit
`Agent: codex-gpt6-astra` attribution. Original tip `d45c844c` remains only
in local backup ref `refs/local-backups/s467-e6746b0025d9-pre-sanitize`.
The original evidence-bearing commits are not ancestors of the new candidate.

Verification: `git diff --exit-code bb543d46 6d00d762 -- pkg` exited 0
with no output. All seven changed Go files (production and tests) are identical;
the recorded red/green test evidence applies unchanged. No code tests were
repeated for this evidence-only correction, as instructed by manager review.
A further harness KB refresh was preserved exactly outside the repository at
`/tmp/s467-e6746b0025d9-resumed-startup-KB.patch` before restoring that file.
No private snapshot is included in either new commit.
