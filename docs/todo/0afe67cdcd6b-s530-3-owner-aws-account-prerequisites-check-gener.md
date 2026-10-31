---
id: 0afe67cdcd6b
kind: feature
title: S530.3 Owner AWS account prerequisites check, generalized setup scripts, static-IP release fix
seq: 2995
status: doing
priority: p1
labels:
    - cloud
    - aws
created: 2026-10-31T00:03:18.891904Z
sprint: 530
sprint_id: 1d6b9478-c054-5b09-961e-c649a3bc1705
sprint_title: 'Master-provisioned AWS satellites: cloudbox provisions Lightsail satellites from owner-held AWS credentials (S522.5)'
---

Split from S522.5 (operator 2026-10-30). The adapter never creates the staging bucket dhnt-userdata-<acct>-<region> and its budget preflight fails closed without a budget, so a new owner's account fails. (1) Before create, the master checks the owner's account and names exactly what is missing (budget dhnt-monthly-cap, staging bucket, IAM scope, region us-west-2). (2) Generalize docs/sprint-522/aws-budget-guard.sh and aws-userdata-bucket.sh to any account (account from STS, no hard-coded DHNT id); CloudShell as root is the cloud-only path. (3) Fix todo 1d1b54a8bb68: destroy leaves <name>-ip allocated and billed because static IPs are untaggable; allow ReleaseStaticIp/DetachStaticIp only for the satellite's own <name>-ip (adapter-side name check) and update the guard. Gate: prerequisite check fails clearly on a fake empty account; fake destroy releases the static IP. Depends on S530.2.

## Conductor brief (s530-manager, 2026-10-30)

Home: yoke `pkg/provider/aws` (nested stdlib-only module: test with `cd pkg/provider && go test ./...`; never `go test ./pkg/provider/` from the root). Region us-west-2 only. Cloudbox (master) already registers this adapter (Sprint 530 S530.1/S530.2, cloudbox dev eb10b176) and surfaces any provider.Error the adapter returns on create; the conductor bumps cloudbox's yoke pin after you land, so the master check = your adapter check.

Your scope:
1. Account prerequisites check. Add an exported check (e.g. `CheckPrerequisites(ctx) (Prerequisites, *provider.Error)`) on the adapter and run it in the create preflight BEFORE any billable call. It must name EXACTLY what is missing, all at once (not first-failure): (a) budget `dhnt-monthly-cap` (or Config.Budget.Name) absent; (b) staging bucket `UserDataBucketName(account, region)` absent (S3 HeadBucket; 404 = missing, 403 = present-but-unreadable -> say so); (c) the key is the account root (STS ARN ends `:root`) -> refuse, name the scoped-user setup; (d) region not us-west-2 -> refuse. Each missing item names the one-time fix: `docs/satellite-on-aws.md` prerequisites (budget guard script / userdata bucket script, run in AWS CloudShell). Use a stable error code (provider.ErrInvalidRequest or the closest existing) and a message that never contains key material. Reuse existing STS/Budgets/S3 clients in this package.
2. Static-IP release: destroy.go already releases `<name>-ip` with a name/attachment check. Add (or confirm) a fakelightsail-backed test that destroy of an instance with an attached static IP releases it and that destroy never releases an IP whose name is not `<instance>-ip` or that is attached elsewhere. Fix any gap you find. (The IAM policy side is the conductor's: docs/sprint-522/aws-budget-guard.sh.)
3. Gate: red/green tests: prerequisite check fails clearly on a fake EMPTY account (no budget, no bucket) listing both; root-key refusal; non-us-west-2 refusal; happy path with both present; static-IP release test. `cd pkg/provider && go vet ./aws/ && go test ./...` green. Extend fakelightsail minimally if it lacks HeadBucket/DescribeBudget behaviour.

Not yours: cloudbox code, the setup scripts, any live AWS call. Commits carry Sprint/Story/Story-ID trailers; capture test exit codes explicitly (never gate on a pipe).
