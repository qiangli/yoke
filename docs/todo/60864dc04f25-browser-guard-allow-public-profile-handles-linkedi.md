---
id: 60864dc04f25
kind: enhancement
title: 'browser guard: allow public profile handles (LinkedIn/GitHub/X) in fields labelled ''username'''
seq: 3111
status: todo
labels:
    - browser
created: 2026-11-13T03:09:50.891533Z
---

Found 2026-11-12 on application forms (Antler, Founders Inc): the sensitive-input guard refuses any field whose label/placeholder says 'username' (field:label-username), e.g. 'LinkedIn: linkedin.com/in/you, or just your username' and 'GitHub: your-username'. These are public profile handles, not credentials, so every such field had to be handed to the operator. Proposal: treat a 'username' field as a public profile handle (allowed) when it is not part of a login form, i.e. no password input in the same form, no autocomplete=username/current-password, and the label, placeholder or URL pattern names a public profile site (linkedin.com, github.com, x.com/twitter.com, scholar, orcid) or the input type is url; keep refusing login usernames and everything else on the guard list. Test against fakes: a profile-handle field (typed) and a login form (refused).
