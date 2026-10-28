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

Plan: umbrella docs/sprint-522-master-execution-plan.md. New yoke/pkg/provider/aws implementing provider.Adapter (provider/v1 ops validate, capacity, plan, create, inspect, list, start, stop, reboot; destroy only on resources tagged dhnt:sprint) against the Lightsail JSON API with stdlib SigV4 (pkg/provider stays stdlib-only, no AWS SDK). Register in provider.BuiltinFactory (pkg/fleet/infra_provision.go) and make the DO-only preflight provider-aware. Budget preflight: refuse create when plan monthly price + month-to-date spend (Cost Explorer, before credits) exceeds the cap, or the bundle exceeds the per-instance ceiling (default $24/month); never GPU. Wire external/aws ProvisioningPermitted to a real check. Credentials only via the vault (AWS_PROFILE or env pair from bashy secret exec). Tests: provider conformance suite, recorded-response unit tests incl. SigV4 vectors, one live create->inspect->destroy of the cheapest bundle after S522.1. cloudbox hub/internal/provisioning registration is a follow-up for S522.5.
