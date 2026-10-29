---
id: ed3ef42edf96
kind: feature
title: S522.2 AWS provider adapter (Lightsail) + bashy infra provision --provider aws with budget preflight
seq: 2816
status: done
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
closed: 2026-10-28T20:08:21.946187Z
closed_by: s522-manager
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

REWORK 2 (manager, 2026-10-28; build on yoke main 3036157e):
Live provision of zoo-aws refused before any AWS call: "user-data script is 43392 bytes and still 21753 bytes after gzip+base64, over the 16384 byte limit". The master's first-boot script (enroll bundle user_data) is ~43 KB and will grow; compression cannot fit 16 KB. Implement the standard AWS pattern — stage the script in S3 and ship a tiny fetch bootstrap:
- When the (wrapped) script exceeds UserDataLimit, PutObject the ORIGINAL script bytes to bucket `dhnt-userdata-<accountId>-<region>` (flag --userdata-bucket / env AWS_USERDATA_BUCKET overrides; accountId from STS), key `<instance>/<random-16-hex>.sh`, SSE AES256 header. The bucket already exists: private, public-access blocked, SSE default, lifecycle expires objects after 1 day; IAM allows dhnt-automation s3:PutObject/GetObject/DeleteObject on bucket/* only (no ListBucket, no CreateBucket).
- User-data becomes a small `#!/bin/sh` bootstrap (well under 16 KB): fetch a SigV4 query-presigned GET URL (X-Amz-Expires 1800; implement presign with the existing stdlib signer, virtual-hosted URL https://<bucket>.s3.<region>.amazonaws.com/<key>) with curl, falling back to wget, retrying for up to 10 minutes (network may lag at first boot); verify sha256 of the fetched file against the value embedded in the bootstrap; exec it with sh; on mismatch/failure write a clear message to /var/log/dhnt-userdata.log and exit non-zero. Never put secrets in the bootstrap other than the presigned URL.
- After the instance reaches running, keep the object (the VM may still be fetching); destroy deletes `<instance>/` objects it knows about (record the key in a tag-free way: deterministic key list isn't available without ListBucket, so store the object key as the Lightsail instance tag dhnt:userdata-key). Lifecycle cleans the rest.
- Keep the in-limit path unchanged (gzip wrapper when it fits, raw when it fits).
- Unit tests: presign known-answer (AWS S3 presigned URL test vector), bootstrap renders < 4 KB and contains sha256, oversize path calls PutObject then CreateInstances with the bootstrap, fakelightsail gets a fake S3 PutObject/DeleteObject. Focused packages only (pkg/provider/..., pkg/fleet, external/aws); NOT the CI ratchet. Commit and exit.

REWORK 3 (manager, 2026-10-28; build on yoke main 15fa1e81):
Live: zoo-aws was created (Lightsail, static IP 52.12.110.80) but never enrolled: the master's first-boot script only knows DigitalOcean metadata (fixed separately in cloudbox S522.8, which also lets an explicit *.sslip.io endpoint get built-in TLS). Two gaps on our side:
1. Static IP FIRST + endpoint hint: for provider aws, `bashy infra provision` must AllocateStaticIp "<name>-ip" (or reuse it if it already exists and is unattached) BEFORE requesting the enrollment bundle, then send endpoint "https://<a-b-c-d>.sslip.io" (dashes) as the bundle endpoint hint unless --endpoint was given, then CreateInstances and AttachStaticIp as today. The satellite then never depends on cloud metadata or on the attach racing first boot. Print the endpoint.
2. Lifecycle verbs: add `bashy infra inspect|list|destroy --provider aws [--name N] [--region R]` (thin CLI over the adapter's existing inspect/list/destroy ops; destroy requires --yes, refuses resources without dhnt:managed=true, deletes the instance, detaches+releases "<name>-ip", deletes the staged S3 object named in tag dhnt:userdata-key, and reports each step). If DO has equivalents already, keep the flag shape consistent; do not change DO behaviour.
Tests: fake-backed unit tests for the order (allocate -> bundle(endpoint) -> create -> attach), reuse of an existing unattached static IP, destroy happy path + untagged refusal + already-gone idempotence. Focused packages only; not the CI ratchet. Commit and exit.

REWORK 4 (manager, 2026-10-28 11:30 PDT; live bugs, build on yoke main):
1. STALE REPLAY: after `bashy infra destroy --provider aws --name zoo-aws`, `bashy infra provision aws --satellite zoo-aws` (default key infra-provision-zoo-aws) printed "provisioned zoo-aws (running)" but made NO AWS call — the provider journal replayed the old create result for an instance that no longer exists (Lightsail GetOperations shows no CreateInstance). Fix: a replayed create must re-inspect the resource and, if it is gone, treat the journal entry as dead and create anew; and/or destroy must retire the journal entries for that resource. Unit test: create -> destroy -> create with the same key really calls CreateInstances.
2. EARLY RETURN: with a fresh key the create returned "provisioned zoo-aws (unknown)" while the instance was still `pending`; firewall (PutInstancePublicPorts) and AttachStaticIp were NOT applied (Lightsail defaults: 80 + 22 open to the world). Manager fixed the live instance by hand. Fix: finish() must wait until running (bounded, with the existing tolerateMissing) before putPorts + ensureStaticIP; if it times out, return an error that names the steps not done, never success. Unit test with fakelightsail returning pending for N polls.
Focused packages only; commit and exit.
