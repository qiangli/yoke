---
id: 01a18725-9931-7bc3-90c8-79515ef2b84b
seq: 11
form: page
type: gotcha
title: 'Account observation hook: where it can and cannot live'
description: WHEN wiring an automatic account-collection hook (pkg/accounts) into a provider or budget path in yoke — pkg/provider is a stdlib-only nested module and pkg/llmbudget is imported BY accounts, so neither can call the hook directly
tags:
    - accounts
    - imports
    - nested-module
status: candidate
evidence: S524.6, pkg/accounts/hook.go + pkg/llmbudget/observe.go (AccountObserver)
source:
    tool: claude-opus5-p
    episode: ep-1d4f3f7981827e60
    parent_episode: ep-9541c4b40407d311
created: "2026-10-29T10:12:30Z"
---

pkg/accounts.ObserveSuccess cannot be called from everywhere the facts are proven:

- pkg/provider (and pkg/provider/{aws,digitalocean}) is a NESTED, stdlib-only module. It cannot import pkg/accounts at all. Wire the hook at the main-module boundary that makes the provider call instead — for the inventory that is each adapter's verified() in pkg/accounts/{aws,digitalocean}.go, which is exactly where the team/account id becomes known and is shared by verify/resources/usage/billing.
- pkg/llmbudget is imported BY pkg/accounts (the LLM adapter reads its cached report), so llmbudget importing accounts is a cycle. It exposes a package-level function variable (llmbudget.AccountObserver) that pkg/accounts installs in its init, mirroring the weavecli.DetectTool / schedule.DefaultAdmission seam pattern.
- Any hook that writes the host inventory needs a go-test guard: pkg/accounts resolves BASHY_HOME and refuses writes inside the operator's real ~/.bashy while testing.Testing(), the same protection pkg/quotaobs has. Without it every adapter unit test silently adds accounts to the live store. Tests that mean to exercise the hook call accounts.SetHookStore(store).
