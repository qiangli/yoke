---
id: 278ade0f220a
kind: enhancement
title: 'browser live mode: detect and report site rate limits and page errors (X ''rate limit exceeded'', HTTP 429, ''Something went wrong'') in the envelope and back off'
seq: 3151
status: todo
priority: p2
labels:
    - browser
created: 2026-11-23T10:02:57.299173Z
---

Observed 2026-11-23 while following ~36 X accounts via bashy browser --mode live (navigate + wait-for-selector + extract + click per profile, ~8 s apart): X rate-limited the founder's session. The loop had no signal: navigate/extract returned normal envelopes and the missing Follow button looked like a slow page. Wanted: (1) the live hub/extension surfaces HTTP 429 and known rate-limit/error page text in the --json envelope (success:false, data.reason=rate_limited|site_error, retry-after when present); (2) wait-for-selector/extract report that signal instead of a timeout; (3) an opt-in --pace or per-host minimum interval so batch drivers pace themselves, with a documented default for x.com/linkedin.com; (4) docs/browser.md gets a 'Rate limits' section. Evidence: S523 follow run script in the agent scratchpad (x-follow.sh), stopped by the founder.
