---
id: ed3ef42edf96
kind: feature
title: S522.2 AWS provider adapter (Lightsail) + bashy infra provision --provider aws with budget preflight
seq: 2816
status: todo
priority: p0
labels:
    - cloud
    - aws
    - provider
created: 2026-10-28T08:13:25.261993Z
sprint: 522
sprint_id: 3413791d-7a40-5514-9970-99b64d1d83cf
sprint_title: 'AWS cloud integration and satellite hosting: bashy aws provider (Lightsail), budget guard, hosting research, self-hosted + hosted AWS satellites'
---

Plan: umbrella docs/sprint-522-master-execution-plan.md (Budget guard section is binding).

GOAL: `bashy infra provision --provider aws` creates, inspects, lists and destroys AWS Lightsail instances through a new built-in provider adapter, with a budget preflight that refuses spending over the cap.

WHERE (yoke):
- Interface: pkg/provider/contract.go (Adapter{Manifest(); Do(ctx, Request)}; provider/v1 ops). Read pkg/provider/README.md.
- Template to mirror: pkg/provider/digitalocean/ (digitalocean.go New/Do, offers.go, hardening.go, trash.go). It calls the REST API directly; do the same.
- Conformance: pkg/provider/conformance/ — the new adapter must pass it.
- Registration: pkg/fleet/infra_provision.go (DO hard-coded around lines 59, 79, 87-89; DO-only preflight ~285) — make it provider-aware and add "aws".
- external/aws/auth.go: AWS_PROFILE required + one credential source; ProvisioningPermitted always refuses — wire it to the real budget preflight below.

WHAT:
1. New package pkg/provider/aws: Lightsail JSON API (POST https://lightsail.<region>.amazonaws.com/, X-Amz-Target: Lightsail_20161128.<Op>, Content-Type application/x-amz-json-1.1) with a stdlib SigV4 signer (pkg/provider stays stdlib-only, NO AWS SDK). Ops: validate, capacity (GetBundles/GetBlueprints/GetRegions), plan, create (CreateInstances with userData, blueprint ubuntu_24_04, tags), inspect/list (GetInstance(s)), start, stop, reboot, destroy (DeleteInstance). Open ports 80/443 via PutInstancePublicPorts when the request asks.
2. Credentials: env AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY (projected by `bashy secret exec AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY -- ...`), region default us-west-2 (AWS_REGION overrides). Never log keys; reuse external/aws/redact.go.
3. Tags: every created resource gets dhnt:managed=true plus dhnt:sprint / dhnt:story / dhnt:ttl when supplied. The IAM policy only allows deleting dhnt:managed=true resources — destroy must fail clearly on untagged ones.
4. Budget preflight before create: monthly price of the bundle (GetBundles price) + month-to-date unblended cost BEFORE credits (Cost Explorer GetCostAndUsage at ce.us-east-1.amazonaws.com, X-Amz-Target AWSInsightsIndexService.GetCostAndUsage, Filter excluding RECORD_TYPE Credit) must stay <= cap (default 50 USD, flag/env override), and the bundle price <= per-instance ceiling (default 24 USD). Refuse otherwise with a clear message. No GPU ever.
5. Lightsail is x86_64 only (good: the satellite installer is amd64-only).

TESTS (focused, run them): SigV4 known-answer vectors (AWS published test suite values), recorded-response unit tests for each op incl. AccessDenied and budget refusal, conformance suite, infra_provision provider selection. Do NOT create real AWS resources — the manager runs the single live create->inspect->destroy with the vault keys after review.

OUT OF SCOPE: EC2, cloudbox hub/internal/provisioning registration (S522.5 follow-up), bashy release. Keep the change to yoke (+ bashy wiring only if `bashy infra` needs a flag change there).
