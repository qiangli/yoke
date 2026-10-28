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
assignee: claude-opus5
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

ADDED BY MANAGER (binding; this is how zoo-aws gets provisioned):
6. The real caller is `bashy infra provision --satellite <id> --provider aws --region us-west-2 --size <bundleId> --image ubuntu_24_04` (pkg/fleet/infra_provision.go -> pkg/provider/enroll.ProvisionExecutable): the master returns an enrollment bundle whose user_data is the COMPLETE first-boot script (pkg/provider/enroll/enroll.go header). The adapter must pass Spec.UserData through byte-identical as Lightsail CreateInstances userData. The script is ~25-35 KB: verify Lightsail's userData size limit from AWS docs; if it is smaller than the script, wrap it losslessly (gzip+base64 into a tiny `#!/bin/sh` bootstrap that decodes and execs it) and unit-test the round trip.
7. Static IP: on create, AllocateStaticIp "<instance>-ip" + AttachStaticIp once the instance is running, so the satellite's sslip.io endpoint survives stop/start; report that IP in inspect; destroy releases it (both tagged dhnt:managed=true; static IPs cannot carry tags in Lightsail, so destroy releases only the IP named "<instance>-ip" attached to that instance).
8. Firewall (PutInstancePublicPorts after create): tcp 80, 443, 6443, 10250; udp 8475-8476; tcp 22 restricted to cidrListAliases ["lightsail-connect"] (browser SSH only). Mirror of the DO `dhnt-satellite` firewall.
9. infra_provision's size preflight / flag defaults are DO-shaped (r1, s-1vcpu-1gb, img): make them provider-aware so the aws defaults are us-west-2 / small_3_0 / ubuntu_24_04, and --dry-run prints the aws request + budget preflight result.

REWORK 1 (manager, 2026-10-28; the adapter itself is merged in yoke main 65ccfb3d — build on it, do not redo it):
Live dry-run against the real account: the budget preflight refuses because Cost Explorer returns HTTP 400 DataUnavailableException on this new account (no data ingested yet). Make the PRIMARY spend source the AWS Budget itself: Budgets DescribeBudget (https://budgets.amazonaws.com, SigV4 region us-east-1 service budgets, X-Amz-Target AWSBudgetServiceGateway.DescribeBudget, AccountId from STS GetCallerIdentity) for budget name 'dhnt-monthly-cap' (new flag --budget-name, default dhnt-monthly-cap): use CalculatedSpend.ActualSpend as month-to-date (that budget is IncludeCredit=false, i.e. before credits) and BudgetLimit as the cap when --budget-cap is not given. Fall back to Cost Explorer only when the budget does not exist (NotFoundException); refuse only when BOTH are unavailable. IAM already allows budgets:ViewBudget. Unit tests (fake): budget source used; CE DataUnavailable + budget present -> proceeds; both unavailable -> refuses; cap from BudgetLimit. Run only the focused packages (pkg/provider/..., pkg/fleet, external/aws) — NOT the CI ratchet (it outlives the idle timeout). Commit and exit.
