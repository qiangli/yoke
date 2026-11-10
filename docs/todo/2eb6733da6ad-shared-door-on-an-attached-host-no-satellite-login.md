---
id: 2eb6733da6ad
kind: bug
title: 'Shared door on an attached host: no satellite login; permission is assumed once paired and shared'
seq: 3092
status: done
priority: p1
labels:
    - llm
    - satellite
created: 2026-11-10T20:47:34.197205Z
assignee: claude-opus5.5-s606
sprint: 606
sprint_id: ebee3343-b774-520a-9d53-b57b306f4210
sprint_title: Streamline Cloudbox master, satellite, and outpost relationships
closed: 2026-11-10T22:14:56.771364Z
closed_by: claude-opus5.5-s606
---

Design rule (operator, 2026-11-10): once an outpost is paired and access is shared, permission is assumed. The satellite puts no login gate in front of a shared app such as the llm door. The door authorizes every request itself:
- grant peers: the credential rides in the client URL path (/k/<cred>, env.OPENAI_BASE_URL);
- member-bound peers (S583.11): identified by the satellite-vouched identity of the authenticated caller.

Found by the S606 final e2e. The fixture had published llm with --require-login (a wrong decision), so the satellite's login gate refused every grant peer with 401 before the door saw it. A real OpenAI client would hit the same wall. Harness fixed (cloudbox 93b4ee4); llm share already publishes the door without login.

Remaining to verify and fix if needed:
1. llm share on an attached host publishes the door so members are identified: no login, with satellite-vouched identity forwarded when the caller is authenticated (outpost apps add --trust-cloud-identity?). Today ensureLocalApp adds the app with --url only.
2. A member-bound peer (llm peer add NAME --email E) is served through the satellite with the member's agent Bearer (200). It is refused without a credential (401/403) and after member removal (403 within 60 s).
3. A real OpenAI client sends Authorization: Bearer <peer key> with the /k/ URL. The satellite must not reject that request before it reaches the door. Confirm the no-login path ignores a non-satellite Bearer.
4. Document the rule in the cloudbox members/attach wire contract.

Acceptance: e2e llm-door and door-by-member PASS on the Linux test host; the unit tests for share publishing and Bearer passthrough on no-login apps are red before and green after.
