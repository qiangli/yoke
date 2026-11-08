---
id: 02159bd275d2
kind: feature
title: S586.2 Share the Bedrock-backed door with non-owner satellite members (llm share --via cloudbox)
seq: 3064
status: done
priority: p2
labels:
    - llm
created: 2026-11-07T18:10:57.113617Z
assignee: claude-opus5.5-s586
sprint: 586
sprint_id: 0d040542-da67-5fa5-917b-6d3a22bdfe3c
sprint_title: AWS Bedrock via a host-minted short-term key
closed: 2026-11-08T02:00:51.165859Z
closed_by: claude-opus5.5-s586
---

Usage: an owner shares their host's LLM door (Bedrock-backed via S586.1) with invited non-owners on a satellite (family, friends, team; first case a class). Members may have no outpost and call the satellite with only a credential.

Existing mechanism, no new code needed: `bashy llm share PEER --via cloudbox --print` publishes the door as the host's public satellite app `llm` and mints a revocable per-peer credential; the member uses https://<satellite>/matrix/h/<host>/app/llm/k/<cred>/v1 (OpenAI) or .../anthropic, with the same credential as the API key. An extra satellite is served by `outpost-profile up <satellite> --app llm`.

Verified 2026-11-07 on zoo-aws (lern-si satellite removed; recreate on AWS later), dev host's zoo-aws instance, a throwaway peer credential:
- GET /v1/models 200 (190 models); wrong credential 401.
- POST /v1/chat/completions 200 routed to a real agent as peer:<name> (door audit records the peer principal).
- POST without an Authorization header gets the satellite's CSRF 403; clients must send `Authorization: Bearer <cred>`, which every OpenAI/Anthropic SDK does with the credential as API key.

Remaining (small):
1. llm share's grant URL always comes from the main outpost's satellite; for an extra-instance satellite the printed URL is wrong. Smallest fix: `llm share --satellite NAME` takes the URL from that satellite profile. Coordinate with the multi-satellite owner (S565) first.
2. Deploy: the host's door runs an older bashy without the Bedrock seeds and the secrets !CMD binding; restart it on the current bashy so Bedrock agents are listed and keys are minted per worker.
3. Live Bedrock 200 through the satellite as a member: waits for the AWS quota case.

Not S586 (reported separately): host-local test agent benchbot-dry captures band L1 and hangs (503); opencode-glm agents return empty content on this host's door for owner and peer alike.

Acceptance: a non-owner with only a credential gets a Bedrock-backed reply through the satellite /v1; the grant names the right satellite URL; native and owner paths unchanged.
