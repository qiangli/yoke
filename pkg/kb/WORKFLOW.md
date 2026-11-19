# Group outcome learning

`bashy kb workflow -f report.json` previews a proposal without writing.
`--apply` fails closed: the CLI has no trusted complete-workflow resolver yet.
Integrators call `ProposeWorkflow(dir, ring, report, apply, verifier)` with a
trusted `WorkflowVerifier` that independently resolves **every** report against
completed-workflow evidence and binds workflow, exact candidate, reported
outcome and applicability. A nil resolver, rejected evidence, or any unverified
or cancelled outcome refuses storage. A caller-supplied boolean is not evidence.
The resolver also checks previously stored reports when merging provenance.

Successful verification writes only an ordinary KB candidate note. It does not
validate knowledge, execute report text, send feedback, learn a skill, or activate
a shaped version. JSON output includes slug, status, deduplication result,
merged provenance and review guidance. Page attribution uses the generic role
`workflow-proposer`; human OS logins and host identities are not stamped.

Example input (illustrative references, not a completed-workflow attestation):

```json
{
  "workflow": "todo:cbb6026e8cdf",
  "candidate": "yoke:add54059",
  "finding": "reconcile-declared-effects-before-retry",
  "kind": "skill",
  "target": "handoff",
  "applicability": "Interrupted group work with declared portable external effects; unknown effects still require reconciliation",
  "reports": [{
    "reporter": "reviewer",
    "episode": "recovery-review",
    "ref": "todo:cbb6026e8cdf",
    "outcome": "unverified",
    "summary": "Review the referenced exact-candidate evidence before reusing this procedure; a package gate alone does not prove live group recovery."
  }]
}
```

Use `kind: incident` for a failure guardrail. Outcomes are **reported**
`passed`, `failed`, `cancelled`, or `unverified`. Every report requires a
reporter, episode, evidence reference and summary. These are supplied claims,
not authenticated identities or verification grants. The preview accepts all reported outcomes; storage requires independently
verified completed outcomes (`passed` or `failed`). The importer rejects
unknown fields, incomplete provenance and trailing JSON. Shareable rings use
the existing privacy check, including provenance and references.

The canonical key includes workflow, exact candidate, stable finding ID,
kind, target and applicability. Reporters of the same finding must reuse
these fields. Wording differences in report summaries do not create another
proposal. Identical reports are no-ops; distinct reports join the provenance
set, preserving contradictory outcomes rather than voting them into truth.
Changed candidates or applicability deliberately create separate candidates.
There is no semantic/LLM deduplication across differently named findings.
Concurrent imports serialize through the existing kernel lock primitive.

The importer only appends provenance while the note remains a candidate.
A later report on a validated, stale or superseded note requires explicit
review/supersession; an identical retry leaves that record untouched. Ordinary
`kb validate` remains a separate evidence-bearing action and serializes with
workflow import. No report outcome fills the KB validation evidence field.
The integrating verifier must resolve source identity, actual workflow gate
result, applicability and unresolved failures independently of the submitted
report. A package gate alone does not establish completion of a group workflow.

Reuse existing downstream channels after review:

- Skill proposals go through `skill learn`/`skill run` contract verification and
  environment applicability, then the existing reviewed promotion procedure.
- Incidents go through `shape incident` or `shape wish`, retaining the KB slug
  as evidence. Existing shape verification, preview, approval and undo apply.
- Designated feedback remains owned by **todo:cdae01f261b1**: existing
  `agentic.<command>` bus messages and human inbox references point to the KB
  candidate rather than spawning another learning store or backlog. Import
  never sends these messages implicitly or resolves the linked todo.

Sprint 666 evidence boundaries: S1 supplies explicit adoption and result-room
references; S5 supplies candidate, ownership and external-effect continuity;
S10 supplies the actual complete workflow gate and baseline measurements.
Record each source separately. Until S10 evidence exists, preview only; do not store a proposal or claim a
completed workflow or measured improvement. Package tests validate the importer mechanics, not those live
workflows. Test fixtures in `workflow_test.go` are synthetic reports.
