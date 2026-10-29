---
id: 9cac261d8d6e
kind: bug
title: 'S525.A5w-oci yoke: @sandbox() zero setup (default bashy image) + bare @sandbox never runs unsandboxed'
seq: 2919
status: todo
priority: p1
labels:
    - software-shaping
created: 2026-10-29T16:54:32.383847Z
sprint: 525
sprint_id: 9a9a4443-02c8-53a3-b0bd-96bd67980643
sprint_title: 'Software Shaping foundation: verified personal features and same-host promotion'
---

Sprint 525, goal a5w. Follow-up bugs from A5w (yoke c9e58916), found in the manager review.
(1) Zero setup: a unit that declares @sandbox() is routed to `bashy oci run`, but it needs $BASHY_SANDBOX_IMAGE set by hand. With no default, the unit is refused with 503. A manual step is a bug. Default to bashy's own runtime image (the FROM-scratch bashy image `bashy self image` builds for the running release), built or fetched automatically on first use with no user action, on every OS bashy oci supports.
(2) A bare `@sandbox` (no parens) is silently ignored by the Bash# parser, so an app that asks for isolation runs without it. Recognise the bare form as @sandbox(), or refuse the unit with a clear error. Never run it unsandboxed.
Acceptance: unit tests with a fake oci runner. (a) With no env var set, @sandbox() resolves to the default bashy image reference and the route runs it. (b) A bare @sandbox unit routes to oci, or is refused, and never runs directly. Keep a fake runner; no podman on Dragon. Sprint/Story/Story-ID trailers; no hostnames.
